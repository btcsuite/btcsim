// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"github.com/conformal/btcutil"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// For now, hardcode a single already-running btcd connection that is used for
// each actor. This should be changed to start a new btcd with the --simnet
// flag, and each actor can connect to the spawned btcd process.
var defaultChainServer = ChainServer{
	connect: "localhost:18556", // local simnet btcd
	user:    "michalis",
	pass:    "kbxkwb",
}

type btcdCmdArgs struct {
	rpcUser string
	rpcPass string
	rpcCert string
	rpcKey  string
}

func (p *btcdCmdArgs) args() []string {
	return []string{
		"--simnet",
		"-u" + p.rpcUser,
		"-P" + p.rpcPass,
		"--rpccert=" + p.rpcCert,
		"--rpckey=" + p.rpcKey,
	}
}

func main() {
	actors := make([]*Actor, 0, 1) // Set cap to expected num of actors run

	btcdHomeDir := btcutil.AppDataDir("btcd", false)
	cert, err := ioutil.ReadFile(filepath.Join(btcdHomeDir, "rpc.cert"))
	if err != nil {
		log.Fatalf("Cannot read certificate: %v", err)
	}
	// Don't change the following assignments to a composite literal since defaultChainServer's
	// already initialized fields will be re-initialized to their zero values.
	defaultChainServer.certPath = filepath.Join(btcdHomeDir, "rpc.cert")
	defaultChainServer.keyPath = filepath.Join(btcdHomeDir, "rpc.key")
	defaultChainServer.cert = cert

	cmdArgs := &btcdCmdArgs{
		rpcUser: defaultChainServer.user,
		rpcPass: defaultChainServer.pass,
		rpcCert: defaultChainServer.certPath,
		rpcKey:  defaultChainServer.keyPath,
	}

	log.Println("Starting btcd on simnet...")
	if err := exec.Command("btcd", cmdArgs.args()...).Start(); err != nil {
		log.Fatalf("Couldn't start btcd: %v", err)
	}

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
	a, err := NewActor(&defaultChainServer, 18554)
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
