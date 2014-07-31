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
	wg               sync.WaitGroup
	upstream         chan btcutil.Address
	downstream       chan btcutil.Address
	timeReceived     chan time.Time
	stop             chan struct{}
	fail             chan struct{}
	interrupt        chan struct{}
	waitForInterrupt chan struct{}
	errChan          chan struct{}
	start            chan struct{}
	txpool           chan struct{}
	txErrChan        chan error
}

// NewCommunication creates a new data structure with all the
// necessary primitives for a fully functional simulation to
// happen.
func NewCommunication() *Communication {
	return &Communication{
		upstream:         make(chan btcutil.Address, *maxActors),
		downstream:       make(chan btcutil.Address, *maxActors),
		timeReceived:     make(chan time.Time, *maxActors),
		stop:             make(chan struct{}, *maxActors),
		fail:             make(chan struct{}),
		interrupt:        make(chan struct{}),
		waitForInterrupt: make(chan struct{}),
		errChan:          make(chan struct{}, *maxActors),
		txErrChan:        make(chan error, *maxActors),
	}
}

// Start handles the main part of a simulation by starting
// all the necessary goroutines.
func (com *Communication) Start(actors []*Actor, client *rpc.Client, btcd *exec.Cmd, txCurve []*Row) (tpsChan chan float64) {
	tpsChan = make(chan float64, 1)

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
			case <-com.fail:
				// All actors have aborted the simulation
				close(tpsChan)
				return
			case <-com.interrupt:
				// Interrupt received
				close(tpsChan)
				<-com.waitForInterrupt
				return
			default:
			}
		}
	}

	currentBlock, err := client.GetBlockCount()
	if err != nil {
		log.Printf("Cannot get block count: %v", err)
	}

	// Start mining.
	miner, err := NewMiner(addressTable, com.stop, com.start, com.txpool, int32(currentBlock))
	if err != nil {
		com.Shutdown(miner, actors, btcd)
		close(com.stop) // make failedActors goroutine exit
		close(tpsChan)
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
	go com.estimateTps(tpsChan)

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
				close(com.fail)
				return
			}
		case <-com.stop:
			// Normal simulation exit
			return
		case <-com.interrupt:
			// Interrupt received
			return
		}
	}
}

// estimateTps estimates the average transactions per second of
// the simulation.
func (com *Communication) estimateTps(tpsChan chan<- float64) {
	defer com.wg.Done()

	var first, last time.Time
	var txnCount int
	firstTx := true

	for {
		select {
		case last = <-com.timeReceived:
			if firstTx {
				first = last
				firstTx = false
			}
			txnCount++
		case <-com.stop:
			// Normal simulation exit
			diff := last.Sub(first)
			tpsChan <- float64(txnCount) / diff.Seconds()
			return
		case <-com.fail:
			close(tpsChan)
			// All actors have aborted the simulation
			return
		case <-com.interrupt:
			close(tpsChan)
			// Interrupt received
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
			case <-com.stop:
				// Normal simulation exit
				return
			case <-com.fail:
				// All actors have aborted the simulation
				return
			case <-com.interrupt:
				// Interrupt received
				<-com.waitForInterrupt
				return
			}
		case <-com.stop:
			// Normal simulation exit
			return
		case <-com.fail:
			// All actors have aborted the simulation
			return
		case <-com.interrupt:
			// Interrupt received
			<-com.waitForInterrupt
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
			if err := miner.client.SetGenerate(false, 0); err != nil {
				log.Printf("Cannot call setgenerate: %v", err)
				return
			}
			// each address sent to com.downstream generates 1 tx
			for i := 0; i < row.v; i++ {
				select {
				case addr := <-com.upstream:
					select {
					case com.downstream <- addr:
					case <-com.stop:
						return
					case <-com.interrupt:
						// Interrupt received
						<-com.waitForInterrupt
						return
					}
				}
			}
			for i := 0; i < row.v; i++ {
				// block until either tx pool gets accepted by miner
				// or we receive an error from the actors
				select {
				case <-com.txpool:
				case err := <-com.txErrChan:
					log.Printf("Tx error: %v", err)
					return
				case <-com.interrupt:
					// Interrupt received
					<-com.waitForInterrupt
					return
				}
			}
			// mine the above tx in the next block
			if err := miner.client.SetGenerate(true, 1); err != nil {
				log.Printf("Cannot call setgenerate: %v", err)
				return
			}
		case <-com.stop:
			return
		case <-com.interrupt:
			// Interrupt received
			<-com.waitForInterrupt
			return
		}
	}
	// done with the curve, so stop the simulation
	close(com.stop)
	return
}

// Shutdown shuts down the simulation by killing the mining and the
// initial btcd processes and shuts down all actors.
func (com *Communication) Shutdown(miner *Miner, actors []*Actor, btcd *exec.Cmd) {
	defer com.wg.Done()

out:
	for {
		select {
		case <-com.stop:
			break out
		case <-com.fail:
			break out
		case <-com.interrupt:
			return
		}
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
