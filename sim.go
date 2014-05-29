// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"os"
	"time"
)

// For now, hardcode a single already-running btcd connection that is used for
// each actor. This should be changed to start a new btcd with the --simnet
// flag, and each actor can connect to the spawned btcd process.
var defaultChainServer = ChainServer{
	connect: "localhost:18334", // local testnet btcd
	user:    "fillmein",
	pass:    "fillmein",
}

func main() {
	actors := make([]*Actor, 0, 1) // Set cap to expected num of actors run

	// If we panic somewhere, at least try to stop the spawned wallet
	// processes.
	defer func() {
		if r := recover(); r != nil {
			log.Println("Panic! Shuting down actors...")
			for _, a := range actors {
				func() {
					// Ignore any other panics that may
					// occur during panic handling.
					defer recover()
					a.Stop()
					a.Cleanup()
				}()
			}
			panic(r)
		}
	}()

	// Create actor.
	a, err := NewActor(&defaultChainServer, 18444)
	if err != nil {
		log.Fatalf("Cannot create actor: %v", err)
	}
	actors = append(actors, a)

	// Start and run for a few seconds.
	if err := a.Start(os.Stderr, os.Stdout); err != nil {
		log.Fatalf("Cannot start actor: %v", err)
	}
	log.Println("Running actor for a few seconds")
	time.Sleep(3 * time.Second)
	log.Println("Time to die")
	if err := a.Stop(); err != nil {
		log.Fatalf("Cannot stop actor: %v", err)
	}
	if err := a.Cleanup(); err != nil {
		log.Fatalf("Cannot cleanup actor directory: %v", err)
	}
	log.Println("Actor shutdown successfully")
}
