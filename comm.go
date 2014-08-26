// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/conformal/btcnet"
	rpc "github.com/conformal/btcrpcclient"
	"github.com/conformal/btcscript"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwire"
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
	enqueueBlock chan *btcwire.ShaHash
	dequeueBlock chan *btcwire.ShaHash
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
		enqueueBlock: make(chan *btcwire.ShaHash),
		dequeueBlock: make(chan *btcwire.ShaHash),
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

	com.wg.Add(1)
	go com.queueBlocks()

	com.wg.Add(1)
	go com.poolUtxos(client, actors)

	// Start a goroutine for shuting down the simulation when appropriate
	com.wg.Add(1)
	go com.Shutdown(miner, actors, btcd)

	return
}

// queueBlocks queues blocks in the order they are received
func (com *Communication) queueBlocks() {
	defer com.wg.Done()

	var blocks []*btcwire.ShaHash
	enqueue := com.enqueueBlock
	var dequeue chan *btcwire.ShaHash
	var next *btcwire.ShaHash
out:
	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no blocks are queued for handling,
				// the queue is finished.
				if len(blocks) == 0 {
					break out
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}
			if len(blocks) == 0 {
				next = n
				dequeue = com.dequeueBlock
			}
			blocks = append(blocks, n)
		case dequeue <- next:
			blocks[0] = nil
			blocks = blocks[1:]
			if len(blocks) != 0 {
				next = blocks[0]
			} else {
				// If no more blocks can be enqueued, the
				// queue is finished.
				if enqueue == nil {
					break out
				}
				dequeue = nil
			}
		case <-com.exit:
			break out
		}
	}
	close(com.dequeueBlock)
}

// poolUtxos receives a new block notification from the chain server
// and pools the newly mined utxos to the corresponding actor's a.utxo
func (com *Communication) poolUtxos(client *rpc.Client, actors []*Actor) {
	defer com.wg.Done()
	// Update utxo pool on each block connected
	for {
		select {
		case blockHash := <-com.dequeueBlock:
			block, err := client.GetBlock(blockHash)
			if err != nil {
				log.Printf("Cannot get block: %v", err)
				return
			}
			// add new outputs to unspent pool
			for i, tx := range block.Transactions() {
			next:
				for n, vout := range tx.MsgTx().TxOut {
					// fetch actor who owns this output
					actor, err := com.getActor(actors, vout)
					if err != nil {
						log.Printf("Cannot get actor: %v", err)
						continue next
					}
					if i == 0 {
						// in case of coinbase tx, add it to coinbase queue
						// if the chan is full, the first tx would be mature
						// so add it to the pool
						select {
						case actor.coinbase <- tx:
							break next
						default:
							// dequeue the first mature tx
							mTx := <-actor.coinbase
							// enqueue the latest tx
							actor.coinbase <- tx
							// we'll process the mature tx next
							// so point tx to mTx
							tx = mTx
							// reset vout as per the new tx
							vout = tx.MsgTx().TxOut[n]
						}
					}
					txout := com.getUtxo(tx, vout, uint32(n))
					// add utxo to actor's pool
					actor.utxo <- txout
				}
			}
		case <-com.exit:
			return
		}
	}
}

// getActor returns the actor to which this vout belongs to
func (com *Communication) getActor(actors []*Actor,
	vout *btcwire.TxOut) (*Actor, error) {
	// get addrs which own this utxo
	_, addrs, _, err := btcscript.ExtractPkScriptAddrs(vout.PkScript, &btcnet.SimNetParams)
	if err != nil {
		return nil, err
	}

	// we're expecting only 1 addr since we created a standard p2pkh tx
	addr := addrs[0].String()
	// find which actor this addr belongs to
	// TODO: could probably be optimized by creating
	// a global addr -> actor index rather than looking
	// up each actor addrs
	for _, actor := range actors {
		if _, ok := actor.addrs[addr]; ok {
			return actor, nil
		}
	}
	err = errors.New("Cannot find any actor who owns this tx output")
	return nil, err
}

// getUtxo returns a TxOut from Tx and Vout
func (com *Communication) getUtxo(tx *btcutil.Tx,
	vout *btcwire.TxOut, index uint32) *TxOut {
	op := btcwire.NewOutPoint(tx.Sha(), index)
	unspent := TxOut{
		OutPoint: op,
		Amount:   btcutil.Amount(vout.Value),
	}
	return &unspent
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
