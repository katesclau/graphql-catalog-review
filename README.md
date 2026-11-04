# Code review exercise: catalog GraphQL API

Time box: 45 minutes. Review this as a pull request that is about to merge to main. Do not rewrite it.

## Scenario

A service needs a small GraphQL API for a program catalog. Programs and airings are stored in MongoDB. Editors create programs and publish them. Clients query a single program or a filtered list, and each program exposes its airings.

The change is the Go code in this directory (`main.go`, `store.go`). There is no existing test suite.

## What to produce

Write review comments the way you would on a real PR:

- Order them by severity.
- For each comment, say what is wrong, why it matters in production, and what change you want.
- Call out missing tests only where a test would lock in a behavior you are worried about.
- Skip formatting, naming nits, and anything a linter would catch.

You may ask clarifying questions. Assume the API is reachable from outside the cluster and more than one instance will run.

## API under review

```graphql
type Program {
  id: String!
  title: String!
  slug: String!
  network: String!
  description: String
  rating: Float
  published: Boolean
  updatedAt: String
  airings: [Airing]
}

type Airing {
  id: String!
  programId: String!
  start: String
  end: String
}

type Query {
  program(id: String!): Program!
  programs(
    filter: String
    search: String
    network: String
    published: Boolean
    sort: String
    page: Int
    limit: Int
  ): [Program!]!
}

type Mutation {
  createProgram(
    title: String!
    slug: String!
    network: String!
    description: String
    rating: Float
    role: String
  ): Program!
  publishProgram(id: String!, role: String): Boolean!
}
```

`programs.filter` is a JSON object encoded as a string. The author says that keeps the query flexible for the web team.
