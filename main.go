// Copyright (c) 2014-2016 The btcsuite developers
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

	"github.com/btcsuite/btcutil"

	"math/rand"
	_ "net/http/pprof"
	"runtime"
	"time"
)

var (
	// maxConnRetries defines the number of times to retry rpc client connections
	maxConnRetries = flag.Int("maxconnretries", 30, "Maximum retries to connect to rpc client")

	// numActors defines the number of actors to spawn
	numActors = flag.Int("actors", 1, "Number of actors to be launched")

	// stopBlock defines how many blocks have to connect to the blockchain
	// before the simulation normally stops
	stopBlock = flag.Int("stopblock", 15000, "Block height to stop the simulation at")

	// startBlock defines after which block the blockchain is start enough to start
	// controlled mining as per the tx curve
	startBlock = flag.Int("startblock", 15000, "Block height to start the simulation at")

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
	// AppDataDir is the path to the working directory set using btcutil.AppDataDir
	AppDataDir = btcutil.AppDataDir("btcsim", false)

	// CertFile is the path to the certificate file of a cert-key pair used for RPC connections
	CertFile = filepath.Join(AppDataDir, "rpc.cert")

	// KeyFile is the path to the key file of a cert-key pair used for RPC connections
	KeyFile = filepath.Join(AppDataDir, "rpc.key")
)

func init() {
	flag.Parse()

	// make sure the app data dir exists
	if !fileExists(AppDataDir) {
		if err := os.Mkdir(AppDataDir, 0700); err != nil {
			log.Fatalf("Cannot create app data dir: %v", err)
		}
	}
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
		log.Printf("Cannot start simulation: %v", err)
		os.Exit(1)
	}
}
