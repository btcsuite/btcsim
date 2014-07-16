// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
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

// Communication is consisted of the necessary primitives used
// for communication between the main goroutine and actors.
type Communication struct {
	upstream   chan btcutil.Address
	downstream chan btcutil.Address
	stop       chan struct{}
}

const connRetry = 15

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(int64(time.Now().Nanosecond()))

	// Number of actors
	var actorsAmount = 1
	actors := make([]*Actor, 0, actorsAmount)
	var wg sync.WaitGroup
	timeReceived := make(chan time.Time, actorsAmount)
	com := Communication{
		upstream:   make(chan btcutil.Address, actorsAmount),
		downstream: make(chan btcutil.Address, actorsAmount),
		stop:       make(chan struct{}, actorsAmount),
	}

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
			timeReceived <- time.Now()
		},
	}

	var client *rpc.Client
	for i := 0; i < connRetry; i++ {
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
		close(exit)
	})

	// Start actors.
	for _, a := range actors {
		wg.Add(1)
		go func(a *Actor, com Communication) {
			defer wg.Done()
			if err := a.Start(os.Stderr, os.Stdout, com); err != nil {
				log.Printf("Cannot start actor on %s: %v", "localhost:"+a.args.port, err)
				a.ForceShutdown()
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

	// tpsChan is used to deliver the average transactions per second
	// of a simulation
	tpsChan := make(chan float64, 1)
	// Start a goroutine to receive transaction times
	go func() {
		var first, last time.Time
		var txnCount int
		firstTx := true

		for {
			select {
			case last = <-timeReceived:
				if firstTx {
					first = last
					firstTx = false
				}
				txnCount++
			case <-com.stop:
				diff := last.Sub(first)
				tpsChan <- float64(txnCount) / diff.Seconds()
				close(tpsChan)
				return
			case <-exit:
				return
			}
		}
	}()

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
			// Normal simulation exit
			break out
		case <-exit:
			// Interrupt handler has finished so exit
			return
		}
	}

	// Stop mining unless the miner is already closed
	if !miner.closed {
		if err := miner.client.SetGenerate(true, 0); err != nil {
			log.Printf("Cannot set miner not to generate coins: %v", err)
		}
	}

	Close(actors, &wg)
	miner.Shutdown()
	if err := Exit(btcd); err != nil {
		log.Printf("Cannot kill initial btcd process: %v", err)
	}

	tps, ok := <-tpsChan
	if ok {
		log.Printf("Average transactions per sec: %.2f", tps)
	}

	// Write info about every simulation in permanent store
	// Current info kept:
	// time the simulation ended: number of actors, transactions per second
	info := fmt.Sprintf("%v: actors: %d, tps: %.2f\n", time.Now(), actorsAmount, tps)

	if _, err := stats.WriteAt([]byte(info), off); err != nil {
		log.Printf("Cannot write to file: %v", err)
	}
	if err := stats.Sync(); err != nil {
		log.Printf("Cannot sync file: %v", err)
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
func Close(actors []*Actor, wg *sync.WaitGroup) {
	defer wg.Wait()
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
