// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/conformal/btcjson"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
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

// Communication is consisted of the necessary primitives used
// for communication between the main goroutine and actors.
type Communication struct {
	upstream   chan btcutil.Address
	downstream chan btcutil.Address
	stop       chan struct{}
	txChan     chan []btcjson.ListTransactionsResult
}

const connRetry = 15

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(int64(time.Now().Nanosecond()))

	// Number of actors
	var actorsAmount = 1
	var wg sync.WaitGroup
	actors := make([]*Actor, 0, actorsAmount)
	com := Communication{
		upstream:   make(chan btcutil.Address, actorsAmount*10),
		downstream: make(chan btcutil.Address, actorsAmount*10),
		stop:       make(chan struct{}, actorsAmount),
		txChan:     make(chan []btcjson.ListTransactionsResult, actorsAmount),
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

	var client *rpc.Client
	for i := 0; i < connRetry; i++ {
		if client, err = rpc.New(&rpcConf, nil); err != nil {
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

	for i := 0; i < actorsAmount; i++ {
		a, err := NewActor(&defaultChainServer, uint16(18557+i))
		if err != nil {
			log.Printf("Cannot create actor on %s: %v", "localhost:"+a.args.port, err)
			continue
		}
		actors = append(actors, a)
	}

	// chan to wait for interrupt handler to finish
	exit := make(chan struct{})
	// close actors and exit btcd on interrupt
	addInterruptHandler(func() {
		Close(actors, &wg)
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		exit <- struct{}{}
	})

	// Start actors.
	for _, a := range actors {
		wg.Add(1)
		go func(a *Actor, com Communication) {
			defer wg.Done()
			if err := a.Start(os.Stderr, os.Stdout, com); err != nil {
				log.Printf("Cannot start actor on %s: %v", "localhost:"+a.args.port, err)
				// TODO: reslice actors when one actor cannot start
			}
		}(a, com)
	}

	addressTable := make([]btcutil.Address, actorsAmount)
	for i, a := range actors {
		select {
		case addressTable[i] = <-com.upstream:
		case <-a.quit:
			// received an interrupt when addresses were being
			// generated. can't continue simulation without addresses
			<-exit
			return
		}
	}

	currentBlock, err := client.GetBlockCount()
	if err != nil {
		log.Printf("Cannot get block count: %v", err)
	}

	// Start mining.
	miner, err := NewMiner(addressTable, com.stop, int32(currentBlock))
	if err != nil {
		Close(actors, &wg)
		if miner != nil { // Miner started so we have to shut it down
			miner.Shutdown()
		}
		if err := Exit(btcd); err != nil {
			log.Printf("Cannot kill initial btcd process: %v", err)
		}
		return
	}

	// cleanup the miner on interrupt
	addInterruptHandler(func() {
		miner.Shutdown()
	})

	// Add mining btcd listen interface as a node
	client.AddNode("localhost:18550", rpc.ANAdd)

out:
	for {
		select {
		case addr := <-com.upstream:
			select {
			case com.downstream <- addr:
			case <-com.stop:
				break out
			}
		case <-com.stop:
			break out
		case <-exit:
			return
		}
	}

	// Stop mining unless the miner is already closed
	if !miner.closed {
		if err := miner.client.SetGenerate(true, 0); err != nil {
			log.Printf("Cannot set miner not to generate coins: %v", err)
		}
	}

	// wait for interrupt handlers to finish
	time.Sleep(50 * time.Millisecond)

	Close(actors, &wg)
	miner.Shutdown()
	if err := Exit(btcd); err != nil {
		log.Printf("Cannot kill initial btcd process: %v", err)
	}

	allTxn := make([]btcjson.ListTransactionsResult, 0)
	for i := 0; i < actorsAmount; i++ {
		// Make receiving from txChan non-blocking in case some actors won't
		// send back any information about transactions.
		select {
		case txn := <-com.txChan:
			allTxn = append(allTxn, txn...)
		default:
		}
	}

	if len(allTxn) == 0 {
		// No info about transactions
		return
	}

	txnBySec := make(map[time.Time]int)
	for _, txn := range allTxn {
		txnBySec[time.Unix(txn.TimeReceived, 0)]++

	}

	var tps int
	for _, t := range txnBySec {
		tps += t
	}
	tps /= len(txnBySec)
	log.Printf("Average transactions per sec: %d", tps)
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

// Close sends close signal to actors and waits for
// all actors to shutdown
func Close(actors []*Actor, wg *sync.WaitGroup) {
	for _, a := range actors {
		a.Stop()
	}
	// wait for actor goroutines to return
	wg.Wait()

	// shutdown only after all actors have stopped
	for _, a := range actors {
		a.Shutdown()
	}
}
