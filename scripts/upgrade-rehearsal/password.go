package main

import (
	"log"
	"os"

	"golang.org/x/crypto/bcrypt"
)

func main() {
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic-upgrade-fixture-password"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(os.Args[1], hash, 0600); err != nil {
		log.Fatal(err)
	}
}
