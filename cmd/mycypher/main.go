package main

import (
	"log"

	"github.com/giovibal/mycypher"
)

func main() {
	db, err := mycypher.Open("data")
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}
	log.Println("mycypher: ok")
}
