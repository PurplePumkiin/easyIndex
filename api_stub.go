//go:build noapi

package main

import "database/sql"

// Stubs so `go build -tags noapi` works while api.go is excluded from the build.

type APIService struct{}

func NewAPIService(*sql.DB, bool, string, string, string, int) *APIService {
	return &APIService{}
}

func (*APIService) Start() {}
