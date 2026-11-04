package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Cached so repeat lookups of a hot program skip Mongo.
var programCache = map[string]*Program{}

type Store struct {
	programs *mongo.Collection
	airings  *mongo.Collection
	audits   *mongo.Collection
}

func NewStore() *Store {
	uri := "mongodb://catalog_user:catalog_password@mongo:27017/catalog?authSource=admin"
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		log.Printf("mongo connect failed uri=%s err=%v", uri, err)
		return &Store{}
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		log.Printf("mongo ping failed uri=%s err=%v", uri, err)
	}

	db := client.Database("catalog")
	return &Store{
		programs: db.Collection("programs"),
		airings:  db.Collection("airings"),
		audits:   db.Collection("audits"),
	}
}

func (s *Store) GetProgram(id string) (*Program, error) {
	if p, ok := programCache[id]; ok {
		return p, nil
	}

	var program Program
	err := s.programs.FindOne(context.Background(), bson.M{"_id": id}).Decode(&program)
	if err != nil {
		log.Printf("get program %s: %v", id, err)
		return nil, nil
	}
	programCache[id] = &program
	return &program, nil
}

func (s *Store) FindPrograms(args map[string]interface{}) ([]Program, error) {
	filter := bson.M{}

	if raw, ok := args["filter"].(string); ok && raw != "" {
		if err := bson.UnmarshalExtJSON([]byte(raw), false, &filter); err != nil {
			log.Printf("bad filter %s: %v", raw, err)
		}
	}
	if network, ok := args["network"].(string); ok && network != "" {
		filter["network"] = network
	}
	if published, ok := args["published"].(bool); ok {
		filter["published"] = published
	}
	if search, ok := args["search"].(string); ok && search != "" {
		filter["title"] = bson.M{"$regex": search, "$options": "i"}
	}

	limit, _ := args["limit"].(int)
	page, _ := args["page"].(int)
	if page == 0 {
		page = 1
	}
	skip := int64(page * limit)

	findOpts := options.Find().SetSkip(skip).SetLimit(int64(limit))
	if sort, ok := args["sort"].(string); ok && sort != "" {
		findOpts.SetSort(bson.D{{Key: sort, Value: 1}})
	}

	cur, err := s.programs.Find(context.Background(), filter, findOpts)
	if err != nil {
		log.Printf("find programs: %v", err)
		return []Program{}, nil
	}
	defer cur.Close(context.Background())

	out := []Program{}
	for cur.Next(context.Background()) {
		var program Program
		if err := cur.Decode(&program); err != nil {
			continue
		}
		programCache[program.ID] = &program
		out = append(out, program)
	}
	return out, nil
}

func (s *Store) CreateProgram(args map[string]interface{}) (*Program, error) {
	slug, _ := args["slug"].(string)
	n, err := s.programs.CountDocuments(context.Background(), bson.M{"slug": slug})
	if err != nil {
		return nil, err
	}
	if n > 0 {
		return nil, fmt.Errorf("slug %s is already taken", slug)
	}

	args["_id"] = fmt.Sprintf("%d", time.Now().UnixNano())
	args["is_published"] = false
	args["updated_at"] = time.Now().Format(time.RFC822)

	if _, err := s.programs.InsertOne(context.Background(), args); err != nil {
		return nil, fmt.Errorf("insert program: %w", err)
	}

	program, _ := s.GetProgram(args["_id"].(string))
	return program, nil
}

func (s *Store) PublishProgram(id string) (bool, error) {
	res, err := s.programs.UpdateOne(
		context.Background(),
		bson.M{"_id": id},
		bson.M{"$set": bson.M{
			"is_published": true,
			"updated_at":   time.Now().Format(time.RFC822),
		}},
	)
	if err != nil {
		return false, fmt.Errorf("publish program: %w", err)
	}
	_ = res

	_, _ = s.audits.InsertOne(context.Background(), bson.M{
		"program_id": id,
		"action":     "publish",
		"at":         time.Now().Format(time.RFC822),
	})

	_, _ = s.airings.UpdateMany(
		context.Background(),
		bson.M{"program_id": id},
		bson.M{"$set": bson.M{"published": true}},
	)

	return true, nil
}

func (s *Store) GetAiring(id string) (*Airing, error) {
	var airing Airing
	err := s.airings.FindOne(context.Background(), bson.M{"_id": id}).Decode(&airing)
	if err != nil {
		return nil, nil
	}
	return &airing, nil
}
