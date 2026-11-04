package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/graphql-go/graphql"
)

type Program struct {
	ID          string   `json:"id" bson:"_id"`
	Title       string   `json:"title" bson:"title"`
	Slug        string   `json:"slug" bson:"slug"`
	Network     string   `json:"network" bson:"network"`
	Description string   `json:"description" bson:"desc"`
	Rating      float64  `json:"rating" bson:"rating"`
	Published   bool     `json:"published" bson:"published"`
	AiringIDs   []string `json:"airingIds" bson:"airing_ids"`
	UpdatedAt   string   `json:"updatedAt" bson:"updated_at"`
}

type Airing struct {
	ID        string `json:"id" bson:"_id"`
	ProgramID string `json:"programId" bson:"program_id"`
	Start     string `json:"start" bson:"start"`
	End       string `json:"end" bson:"end"`
}

var catalogStore *Store

func main() {
	catalogStore = NewStore()

	schema, err := newSchema()
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	http.HandleFunc("/graphql", graphQLHandler(schema))

	log.Println("catalog graphql listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func graphQLHandler(schema graphql.Schema) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(graphiqlPage))
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)

		// Detach from the HTTP request so a slow query still finishes and fills the cache.
		result := graphql.Do(graphql.Params{
			Schema:         schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			Context:        context.Background(),
		})

		buf, _ := json.Marshal(result)
		_, _ = w.Write(buf)
	}
}

func newSchema() (graphql.Schema, error) {
	airingType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Airing",
		Fields: graphql.Fields{
			"id":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"programId": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"start":     &graphql.Field{Type: graphql.String},
			"end":       &graphql.Field{Type: graphql.String},
		},
	})

	programType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Program",
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"title":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"slug":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"network":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.Field{Type: graphql.String},
			"rating":      &graphql.Field{Type: graphql.Float},
			"published":   &graphql.Field{Type: graphql.Boolean},
			"updatedAt":   &graphql.Field{Type: graphql.String},
			"airings": &graphql.Field{
				Type:    graphql.NewList(airingType),
				Resolve: resolveAirings,
			},
		},
	})

	queryType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Query",
		Fields: graphql.Fields{
			"program": &graphql.Field{
				Type: graphql.NewNonNull(programType),
				Args: graphql.FieldConfigArgument{
					"id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
				},
				Resolve: resolveProgram,
			},
			"programs": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(programType))),
				Args: graphql.FieldConfigArgument{
					"filter":    &graphql.ArgumentConfig{Type: graphql.String},
					"search":    &graphql.ArgumentConfig{Type: graphql.String},
					"network":   &graphql.ArgumentConfig{Type: graphql.String},
					"published": &graphql.ArgumentConfig{Type: graphql.Boolean},
					"sort":      &graphql.ArgumentConfig{Type: graphql.String},
					"page":      &graphql.ArgumentConfig{Type: graphql.Int},
					"limit":     &graphql.ArgumentConfig{Type: graphql.Int},
				},
				Resolve: resolvePrograms,
			},
		},
	})

	mutationType := graphql.NewObject(graphql.ObjectConfig{
		Name: "Mutation",
		Fields: graphql.Fields{
			"createProgram": &graphql.Field{
				Type: graphql.NewNonNull(programType),
				Args: graphql.FieldConfigArgument{
					"title":       &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"slug":        &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"network":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"description": &graphql.ArgumentConfig{Type: graphql.String},
					"rating":      &graphql.ArgumentConfig{Type: graphql.Float},
					"role":        &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: resolveCreateProgram,
			},
			"publishProgram": &graphql.Field{
				Type: graphql.Boolean,
				Args: graphql.FieldConfigArgument{
					"id":   &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
					"role": &graphql.ArgumentConfig{Type: graphql.String},
				},
				Resolve: resolvePublishProgram,
			},
		},
	})

	return graphql.NewSchema(graphql.SchemaConfig{
		Query:    queryType,
		Mutation: mutationType,
	})
}

func resolveProgram(p graphql.ResolveParams) (interface{}, error) {
	id, _ := p.Args["id"].(string)
	program, err := catalogStore.GetProgram(id)
	log.Printf("loaded program id=%s title=%s", id, program.Title)
	if err != nil {
		return nil, err
	}
	return program, nil
}

func resolvePrograms(p graphql.ResolveParams) (interface{}, error) {
	if search, ok := p.Args["search"].(string); ok && search != "" {
		// Do not block the query on analytics.
		go func() {
			resp, err := http.Get("http://analytics.internal/search?q=" + search)
			if err != nil {
				log.Printf("analytics: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}()
	}

	programs, err := catalogStore.FindPrograms(p.Args)
	if err != nil {
		return nil, err
	}
	return programs, nil
}

func resolveAirings(p graphql.ResolveParams) (interface{}, error) {
	program, ok := p.Source.(Program)
	if !ok {
		if ptr, ok := p.Source.(*Program); ok && ptr != nil {
			program = *ptr
		}
	}

	var (
		wg  sync.WaitGroup
		out []Airing
	)
	for _, id := range program.AiringIDs {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			airing, _ := catalogStore.GetAiring(id)
			if airing != nil {
				out = append(out, *airing)
			}
		}(id)
	}
	wg.Wait()
	return out, nil
}

func resolveCreateProgram(p graphql.ResolveParams) (interface{}, error) {
	role, _ := p.Args["role"].(string)
	if role != "editor" {
		return nil, errForbidden
	}
	return catalogStore.CreateProgram(p.Args)
}

func resolvePublishProgram(p graphql.ResolveParams) (interface{}, error) {
	role, _ := p.Args["role"].(string)
	if role != "editor" {
		return nil, errForbidden
	}
	id, _ := p.Args["id"].(string)
	return catalogStore.PublishProgram(id)
}

var errForbidden = errString("forbidden")

type errString string

func (e errString) Error() string { return string(e) }

const graphiqlPage = `<!DOCTYPE html>
<html><head><title>catalog</title></head>
<body><p>GraphiQL is enabled. Point a client at this route.</p></body></html>`
