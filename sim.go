// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
)

// ChainServer describes the arguments necessary to connect a btcwallet
// instance to a btcd websocket RPC server.
type ChainServer struct {
	connect  string
	user     string
	pass     string
	certPath string
	keyPath  string
	cert     []byte
}

// For now, hardcode a single already-running btcd connection that is used for
// each actor. This should be changed to start a new btcd with the --simnet
// flag, and each actor can connect to the spawned btcd process.
var defaultChainServer = ChainServer{
	connect: "localhost:18556", // local simnet btcd
	user:    "rpcuser",
	pass:    "rpcpass",
}

var (
	// maxConnRetries defines the number of times to retry rpc client connections
	maxConnRetries = flag.Int("maxconnretries", 15, "Maximum retries to connect to rpc client")

	// maxActors defines the Number of actors to spawn
	maxActors = flag.Int("maxactors", 1, "Maximum number of actors")

	// maxBlocks defines how many blocks have to connect to the blockchain
	// before the simulation normally stops
	maxBlocks = flag.Int("maxblocks", 14000, "Maximum blocks to generate")

	// maxAddresses defines the number of addresses to generate per actor
	maxAddresses = flag.Int("maxaddresses", 10, "Maximum addresses per actor")
)

func init() {
	flag.Parse()

	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(time.Now().UnixNano())
}

func main() {
	actors := make([]*Actor, 0, *maxActors)
	com := NewCommunication()

	// Save info about the simulation in permanent store.
	var stats *os.File
	var err error
	stats, err = os.OpenFile("btcsim-stats.txt", os.O_RDWR, os.ModePerm)
	if os.IsNotExist(err) {
		stats, err = os.Create("btcsim-stats.txt")
		if err != nil {
			log.Printf("Cannot create stats file: %v", err)
		}
	}
	defer stats.Close()

	// Move the offset at the end of the file
	off, err := stats.Seek(0, os.SEEK_END)
	if err != nil {
		log.Printf("Cannot set offset at the end of the file: %v", err)
	}

	btcdHomeDir := btcutil.AppDataDir("btcd", false)
	defaultChainServer.certPath = filepath.Join(btcdHomeDir, "rpc.cert")
	defaultChainServer.keyPath = filepath.Join(btcdHomeDir, "rpc.key")
	cert, err := ioutil.ReadFile(defaultChainServer.certPath)
	if err != nil {
		log.Fatalf("Cannot read certificate: %v", err)
	}
	defaultChainServer.cert = cert

	btcdArgs := []string{
		"--simnet",
		"-u" + defaultChainServer.user,
		"-P" + defaultChainServer.pass,
		"--rpccert=" + defaultChainServer.certPath,
		"--rpckey=" + defaultChainServer.keyPath,
		"--profile=",
	}

	log.Println("Starting btcd on simnet...")
	btcd := exec.Command("btcd", btcdArgs...)
	if err := btcd.Start(); err != nil {
		log.Fatalf("Couldn't start btcd: %v", err)
	}

	// Create and start RPC client.
	rpcConf := rpc.ConnConfig{
		Host:         defaultChainServer.connect,
		Endpoint:     "ws",
		User:         defaultChainServer.user,
		Pass:         defaultChainServer.pass,
		Certificates: defaultChainServer.cert,
	}

	ntfnHandlers := rpc.NotificationHandlers{
		OnTxAccepted: func(hash *btcwire.ShaHash, amount btcutil.Amount) {
			log.Printf("Transaction accepted: Hash: %v, Amount: %v", hash, amount)
			com.timeReceived <- time.Now()
		},
	}

	var client *rpc.Client
	for i := 0; i < *maxConnRetries; i++ {
		if client, err = rpc.New(&rpcConf, &ntfnHandlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}
	if client == nil {
		log.Printf("Cannot start btcd rpc client: %v", err)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		return
	}

	// Register for transaction notifications
	if err := client.NotifyNewTransactions(false); err != nil {
		log.Printf("Cannot register for transactions notifications: %v", err)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		return
	}

	for i := 0; i < *maxActors; i++ {
		a, err := NewActor(&defaultChainServer, uint16(18557+i))
		if err != nil {
			log.Printf("Cannot create actor on %s: %v", "localhost:"+a.args.port, err)
			continue
		}
		actors = append(actors, a)
	}

	// close actors and exit btcd on interrupt
	addInterruptHandler(func() {
		close(com.interrupt)
		Close(actors)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		close(com.waitForInterrupt)
	})

	// Start simulation.
	tpsChan := com.Start(actors, client, btcd)
	com.WaitForShutdown()

	tps, ok := <-tpsChan
	if ok && tps != 0 {
		log.Printf("Average transactions per sec: %.2f", tps)
		// Write info about every simulation in permanent store
		// Current info kept:
		// time the simulation ended: number of actors, transactions per second
		info := fmt.Sprintf("%v: actors: %d, tps: %.2f\n", time.Now(), *maxActors, tps)

		if _, err := stats.WriteAt([]byte(info), off); err != nil {
			log.Printf("Cannot write to file: %v", err)
		}
		if err := stats.Sync(); err != nil {
			log.Printf("Cannot sync file: %v", err)
		}
	}

	dataDir := path.Join(btcdHomeDir, "data")
	simnetDir := path.Join(dataDir, "simnet")
	if err := os.RemoveAll(simnetDir); err != nil {
		log.Printf("Cannot remove simnet data directory: %v", err)
	}

}

// Exit closes the cmd by passing SIGINT
// workaround for windows by passing SIGKILL
func Exit(cmd *exec.Cmd) (err error) {
	defer cmd.Wait()

	if runtime.GOOS == "windows" {
		err = cmd.Process.Signal(os.Kill)
	} else {
		err = cmd.Process.Signal(os.Interrupt)
	}

	return
}

// Close sends close signal to actors, waits for actor goroutines
// to exit and then shuts down all actors.
func Close(actors []*Actor) {
	// Stop actors by shuting down their rpc client and closing quit channel.
	for _, a := range actors {
		a.Stop()
		a.WaitForShutdown()
	}

	// shutdown only after all actors have stopped
	for _, a := range actors {
		a.Shutdown()
	}
}
