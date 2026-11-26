// api is the back end of the todo-stack example: an in-memory todo list over
// JSON. Standard library only, so the build needs no network for modules.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
)

type todo struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
}

var (
	mu    sync.Mutex
	todos = []todo{{ID: 1, Title: "Deploy the todo stack"}}
)

func main() {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	http.HandleFunc("/api/todos", func(w http.ResponseWriter, r *http.Request) {
		// The browser calls this from the web page's origin.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodOptions:
			return
		case http.MethodPost:
			var in todo
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Title) == "" {
				http.Error(w, `{"error":"title is required"}`, http.StatusBadRequest)
				return
			}
			in.ID = len(todos) + 1
			todos = append(todos, in)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(in)
		default:
			json.NewEncoder(w).Encode(todos)
		}
	})
	log.Println("api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
