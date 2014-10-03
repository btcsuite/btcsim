// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/conformal/btcutil"

	"math/rand"
	_ "net/http/pprof"
	"runtime"
	"time"
)

var (
	// maxConnRetries defines the number of times to retry rpc client connections
	maxConnRetries = flag.Int("maxconnretries", 15, "Maximum retries to connect to rpc client")

	// numActors defines the number of actors to spawn
	numActors = flag.Int("actors", 1, "Number of actors to be launched")

	// maxBlocks defines how many blocks have to connect to the blockchain
	// before the simulation normally stops
	maxBlocks = flag.Int("maxblocks", 20000, "Maximum blocks to generate")

	// matureBlock defines after which block the blockchain is mature enough to start
	// controlled mining as per the tx curve
	matureBlock = flag.Int("matureblock", 20000, "Block number at blockchain maturity")

	// maxAddresses defines the number of addresses to generate per actor
	maxAddresses = flag.Int("maxaddresses", 100, "Maximum addresses per actor")

	// maxBlockSize defines the maximum block size to be passed as -blockmaxsize to the miner
	maxBlockSize = flag.Int("maxblocksize", 999000, "Maximum block size in bytes used by the miner")

	// maxSplit defines the maximum number of pieces to divide a utxo into
	maxSplit = flag.Int("maxsplit", 100, "Maximum number of pieces to divide a utxo into")

	// profile
	profile = flag.String("profile", "6060", "Listen address for profiling server")

	// txCurvePath is the path to a CSV file containing the block, utxo count, tx count
	txCurvePath = flag.String("txcurve", "",
		"Path to the CSV File containing block, utxo count, tx count fields")
)

var (
	AppDataDir, CertFile, KeyFile string
)

func init() {
	flag.Parse()

	AppDataDir = btcutil.AppDataDir("btcsim", false)
	if !fileExists(AppDataDir) {
		if err := os.Mkdir(AppDataDir, 0700); err != nil {
			log.Fatalf("Cannot create app data dir: %v", err)
		}
	}
	CertFile = filepath.Join(AppDataDir, "rpc.cert")
	KeyFile = filepath.Join(AppDataDir, "rpc.key")

}

func main() {
	// Seed random
	rand.Seed(time.Now().UnixNano())
	// Use all processor cores.
	runtime.GOMAXPROCS(runtime.NumCPU())

	if *profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", *profile)
			log.Printf("Profile server listening on %s", listenAddr)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			log.Printf("%v", http.ListenAndServe(listenAddr, nil))
		}()
	}

	simulation := NewSimulation()
	simulation.readTxCurve(*txCurvePath)
	simulation.updateFlags()
	if err := simulation.Start(); err != nil {
		os.Exit(1)
	}
}
