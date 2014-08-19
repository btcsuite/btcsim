// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
)

// Communication is consisted of the necessary primitives used
// for communication between the main goroutine and actors.
type Communication struct {
	wg           sync.WaitGroup
	upstream     chan btcutil.Address
	downstream   chan btcutil.Address
	timeReceived chan time.Time
	exit         chan struct{}
	waitForExit  chan struct{}
	errChan      chan struct{}
	start        chan struct{}
	txpool       chan struct{}
}

// NewCommunication creates a new data structure with all the
// necessary primitives for a fully functional simulation to
// happen.
func NewCommunication() *Communication {
	return &Communication{
		upstream:     make(chan btcutil.Address, *maxActors),
		downstream:   make(chan btcutil.Address, *maxActors),
		timeReceived: make(chan time.Time, *maxActors),
		exit:         make(chan struct{}),
		waitForExit:  make(chan struct{}),
		errChan:      make(chan struct{}, *maxActors),
	}
}

// Start handles the main part of a simulation by starting
// all the necessary goroutines.
func (com *Communication) Start(actors []*Actor, client *rpc.Client, btcd *exec.Cmd, txCurve []*Row) (tpsChan chan float64) {
	tpsChan = make(chan float64, 1)

	// Start a goroutine to wait for the simulation to exit
	com.wg.Add(1)
	go com.exitWait()

	// Start actors
	for _, a := range actors {
		com.wg.Add(1)
		go func(a *Actor, com *Communication) {
			defer com.wg.Done()
			if err := a.Start(os.Stderr, os.Stdout, com); err != nil {
				log.Printf("Cannot start actor on %s: %v", "localhost:"+a.args.port, err)
				a.ForceShutdown()
			}
		}(a, com)
	}

	// Start a goroutine to check if all actors have failed
	com.wg.Add(1)
	go com.failedActors()

	addressTable := make([]btcutil.Address, *maxActors)
	for i, a := range actors {
		select {
		case addressTable[i] = <-com.upstream:
		case <-a.quit:
			// This actor has quit
			select {
			case <-com.exit:
				close(tpsChan)
				return
			default:
			}
		}
	}

	// Start mining.
	miner, err := NewMiner(addressTable, com.exit, com.start, com.txpool)
	if err != nil {
		safeClose(com.exit) // make failedActors goroutine exit
		close(tpsChan)
		com.wg.Add(1)
		go com.Shutdown(miner, actors, btcd)
		return
	}

	// cleanup the miner on interrupt
	addInterruptHandler(func() {
		miner.Shutdown()
	})

	// Add mining btcd listen interface as a node
	client.AddNode("localhost:18550", rpc.ANAdd)

	// Start a goroutine to estimate tps
	com.wg.Add(1)
	go com.estimateTps(tpsChan, txCurve)

	// Start a goroutine to coordinate transactions
	com.wg.Add(1)
	if txCurve != nil {
		go com.CommunicateTxCurve(txCurve, miner)
	} else {
		go com.Communicate()
	}

	// Start a goroutine for shuting down the simulation when appropriate
	com.wg.Add(1)
	go com.Shutdown(miner, actors, btcd)

	return
}

// failedActors checks for actors that aborted the simulation
func (com *Communication) failedActors() {
	defer com.wg.Done()

	var failedActors int

	for {
		select {
		case <-com.errChan:
			failedActors++

			// All actors have failed
			if failedActors == *maxActors {
				safeClose(com.exit)
				return
			}
		case <-com.exit:
			return
		}
	}
}

// estimateTps estimates the average transactions per second of
// the simulation.
func (com *Communication) estimateTps(tpsChan chan<- float64, txCurve []*Row) {
	defer com.wg.Done()

	var first, last time.Time
	var diff, curveDiff time.Duration
	var txnCount, curveCount, block int
	firstTx := true

	for {
		select {
		case last = <-com.timeReceived:
			if firstTx {
				first = last
				firstTx = false
			}
			txnCount++
			diff = last.Sub(first)

			// Controlled mining simulation
			if txCurve != nil {
				curveCount++
				if curveCount == txCurve[block].v {
					// A block has been mined; reset necessary variables
					curveCount = 0
					firstTx = true
					curveDiff += diff
					diff = curveDiff
					// Use next block's desired transaction count
					block++
				}
			}
		case <-com.exit:
			tpsChan <- float64(txnCount) / diff.Seconds()
			return
		}
	}
}

// Communicate handles the main part of the communication; receiving
// and sending of addresses to actors ie. enables transactions to
// happen.
func (com *Communication) Communicate() {
	defer com.wg.Done()

	for {
		select {
		case addr := <-com.upstream:
			select {
			case com.downstream <- addr:
			case <-com.exit:
				return
			}
		case <-com.exit:
			return
		}
	}
}

// CommunicateTxCurve generates tx and controls the mining according
// to the input block height vs tx count curve
func (com *Communication) CommunicateTxCurve(txCurve []*Row, miner *Miner) {
	defer com.wg.Done()

	for _, row := range txCurve {
		select {
		case <-com.start:
			// disable mining until the required no. of tx are in mempool
			if err := miner.StopMining(); err != nil {
				safeClose(com.exit)
				return
			}
			var wg sync.WaitGroup
			for i := 0; i < row.v; i++ {
				select {
				case addr := <-com.upstream:
					select {
					case com.downstream <- addr:
						// For every address sent downstream (one transaction about to happen),
						// spawn a goroutine to listen for an accepted transaction in the mempool
						wg.Add(1)
						go com.txPoolRecv(&wg)
					case <-com.exit:
						return
					}
				case <-com.exit:
					return
				}
			}
			wg.Wait()
			// mine the above tx in the next block
			if err := miner.StartMining(); err != nil {
				safeClose(com.exit)
				return
			}
		case <-com.exit:
			return
		}
	}
	// done with the curve, so stop the simulation
	safeClose(com.exit)
	return
}

// Shutdown shuts down the simulation by killing the mining and the
// initial btcd processes and shuts down all actors.
func (com *Communication) Shutdown(miner *Miner, actors []*Actor, btcd *exec.Cmd) {
	defer com.wg.Done()

	select {
	case <-com.exit:
		safeClose(com.waitForExit)
	}

	if miner != nil {
		miner.Shutdown()
	}
	Close(actors)
	if err := Exit(btcd); err != nil {
		log.Printf("Cannot kill initial btcd process: %v", err)
	}
}

// WaitForShutdown waits until every goroutine inside com.Start
// has returned.
func (com *Communication) WaitForShutdown() {
	com.wg.Wait()
}

// txPoolRecv listens for transactions accepted in the miner mempool
// or errors happened during the creation or send of a transaction.
func (com *Communication) txPoolRecv(wg *sync.WaitGroup) {
	defer wg.Done()

	select {
	case <-com.txpool:
	case <-com.exit:
	}
}

// exitWait waits for the simulation to exit
func (com *Communication) exitWait() {
	defer com.wg.Done()

	select {
	case <-com.exit:
		<-com.waitForExit
	}
}
