// Copyright (C) 2016  Lukas Lalinsky
// Distributed under the MIT license, see the LICENSE file for details.

package main

import (
	"flag"
	"fmt"
	"github.com/acoustid/go-acoustid/api/handlers"
	"gopkg.in/mgo.v2"
	"log"
	"net/http"
)

func main() {
	host := flag.String("host", "localhost", "host on which to listen")
	port := flag.Int("port", 8080, "port number on which to listen")
	dbUrl := flag.String("db", "mongodb://localhost/acoustid", "which database to use")

	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	session, err := mgo.Dial(*dbUrl)
	if err != nil {
		log.Fatalf("Could not connect to the database at %s: %s", *dbUrl, err)
	}
	defer session.Close()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "Nothing to see here.\n")
	})

	http.Handle("/v2/submit", handlers.NewSubmitHandler(handlers.NewMongoSubmissionStore(session)))
	http.Handle("/v2/lookup", handlers.NewLookupHandler(session))

	var addr = fmt.Sprintf("%s:%d", *host, *port)

	log.Printf("Listening on http://%s\n", addr)

	log.Fatal(http.ListenAndServe(addr, nil))
}
