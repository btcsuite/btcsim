// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"errors"
	"fmt"
	"log"
	"math"
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

// Block contains the block hash and height as received in a
// OnBlockConnected notification
type Block struct {
	hash   *btcwire.ShaHash
	height int32
}

// Communication is consisted of the necessary primitives used
// for communication between the main goroutine and actors.
type Communication struct {
	wg           sync.WaitGroup
	upstream     chan btcutil.Address
	downstream   chan TxSplit
	timeReceived chan time.Time
	exit         chan struct{}
	errChan      chan struct{}
	height       chan int32
	txpool       chan struct{}

	enqueueBlock chan *Block
	dequeueBlock chan *Block
	processed    chan *Block
}

// NewCommunication creates a new data structure with all the
// necessary primitives for a fully functional simulation to
// happen.
func NewCommunication() *Communication {
	return &Communication{
		upstream:     make(chan btcutil.Address, 10**maxActors),
		downstream:   make(chan TxSplit, *maxActors),
		timeReceived: make(chan time.Time, *maxActors),
		height:       make(chan int32),
		txpool:       make(chan struct{}),
		exit:         make(chan struct{}),
		errChan:      make(chan struct{}, *maxActors),
		enqueueBlock: make(chan *Block),
		dequeueBlock: make(chan *Block),
		processed:    make(chan *Block),
	}
}

// Start handles the main part of a simulation by starting
// all the necessary goroutines.
func (com *Communication) Start(actors []*Actor, client *rpc.Client, btcd *exec.Cmd, txCurve map[int32]*Row) (tpsChan chan float64) {
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
			case <-com.exit:
				close(tpsChan)
				return
			default:
			}
		}
	}

	// Start mining.
	miner, err := NewMiner(addressTable, com.exit, com.height, com.txpool)
	if err != nil {
		safeClose(com.exit) // make failedActors goroutine exit
		close(tpsChan)
		com.wg.Add(1)
		go com.Shutdown(miner, actors, btcd)
		return
	}

	// Add mining btcd listen interface as a node
	client.AddNode("localhost:18550", rpc.ANAdd)

	// Start a goroutine to estimate tps
	com.wg.Add(1)
	go com.estimateTps(tpsChan, txCurve)

	// Start a goroutine to coordinate transactions
	com.wg.Add(1)
	go com.Communicate(txCurve, miner, actors)

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

	var blocks []*Block
	enqueue := com.enqueueBlock
	var dequeue chan *Block
	var next *Block
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
		case b, ok := <-com.dequeueBlock:
			if !ok {
				return
			}
			block, err := client.GetBlock(b.hash)
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
					actor.enqueueUtxo <- txout
				}
			}
			// allow Communicate to sync with the processed block
			if b.height >= int32(*matureBlock)-1 {
				var utxoCount int
				for _, a := range actors {
					utxoCount += len(a.utxos)
				}
				log.Printf("%v: block# %v: no of tx: %v, no of utxos: %v", b.height,
					b.hash, len(block.Transactions()), utxoCount)
				select {
				case com.processed <- b:
				case <-com.exit:
					return
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
func (com *Communication) estimateTps(tpsChan chan<- float64, txCurve map[int32]*Row) {
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

			curveCount++
			if c, ok := txCurve[int32(block)]; ok && curveCount == c.txCount {
				// A block has been mined; reset necessary variables
				curveCount = 0
				firstTx = true
				curveDiff += diff
				diff = curveDiff
				// Use next block's desired transaction count
				block++
			}
		case <-com.exit:
			tpsChan <- float64(txnCount) / diff.Seconds()
			return
		}
	}
}

// Communicate generates tx and controls the mining according
// to the input block height vs tx count curve
func (com *Communication) Communicate(txCurve map[int32]*Row, miner *Miner, actors []*Actor) {
	defer com.wg.Done()

	for {
		select {
		case h := <-com.height:

			// disable mining until the required no. of tx are in mempool
			if err := miner.StopMining(); err != nil {
				safeClose(com.exit)
				return
			}

			// wait until this block is processed
			select {
			case <-com.processed:
			case <-com.exit:
				return
			}

			var wg sync.WaitGroup
			// count the number of utxos available in total
			var utxoCount int
			for _, a := range actors {
				utxoCount += len(a.utxos)
			}

			// txSplit maps the number of 'splits' to the number of tx in each
			// split category
			// a split is necessary because we need some tx to contribute the utxo
			// count required for the next block and the rest to contribute to the tx count
			//
			// it is possible to keep dividing the same utxo until it's broken into the required
			// number of pieces but we want to stay close to the real world scenario and maximize
			// the number of utxos used
			//
			// E.g: Assume the following CSV
			//
			// block,utxos,tx
			// 20000,40000,20000
			// 20001,50000,25000
			//
			// at block 19999, we need to ensure that next block has 40K utxos
			// we have 19999 - btcchain.CoinbaseMaturity = 19899 utxos
			// we need to create 40K-19899 = 20101 utxos so in this case the split is:
			// {1: 20101} i.e we need to create 20101 tx which give 1 net utxo output
			//
			// at block 20000, we need to ensure that next block has 50K utxos
			// we already have 40K by the previous iteration, so we need 50-40 = 10K utxos
			// we also need to generate 20K tx before the next block, so the split is:
			// {0: 10000,1: 10000} i.e. we need to create 10000 tx which generate 1 net utxo
			// plus 10000 tx which don't generate any net utxo
			//
			// since we cannot generate more tx than the no of available utxos, the no of tx
			// that can be generated at any iteration is limited by the utxos available
			txSplit := make(map[int]int)

			// in case the next row doesn't exist, we initialize the required no of utxos to zero
			// so we keep the utxoCount same as current count
			next, ok := txCurve[h+1]
			if !ok {
				next = &Row{}
				next.utxoCount = utxoCount
			}

			// reqUtxoCount is the number of utxos required
			reqUtxoCount := next.utxoCount - utxoCount

			// in case this row doesn't exist, we initialize the required no of tx to reqUtxoCount
			// i.e one tx per utxo required
			row, ok := txCurve[h]
			if !ok {
				row = &Row{}
				row.txCount = reqUtxoCount
			}

			// reqTxCount is the number of tx that will generate reqUtxoCount
			// no of utxos
			reqTxCount := row.txCount
			if reqTxCount > utxoCount {
				log.Printf("Warning: capping no of tx at %v based on no of available utxos", utxoCount)
				// cap the total no of tx at the no of available utxos
				reqTxCount = utxoCount
			}

			// skip if we already have more than the no of utxos required
			if reqUtxoCount > 0 {
				// e.g: if we need 18K utxos in 12K tx
				// multiplier = [18000/12000] = [1.5] = 2
				// txSplit[2] = 18000/2 = 9000
				// txSplit[0] = 120000 - 9000 = 3000
				// txSplit = {0: 3000, 2: 9000}
				multiplier := int(math.Ceil(float64(reqUtxoCount) / float64(reqTxCount)))
				txCount := reqUtxoCount / multiplier
				txSplit[multiplier] = txCount
				if reqTxCount > txCount {
					txSplit[0] = reqTxCount - txCount
				}
			}

			log.Printf("- row %v - utxos - %v - txs - %v", h, next.utxoCount, row.txCount)
			log.Printf("Need to generate %v utxos and %v tx", reqUtxoCount, reqTxCount)
			log.Printf("Splitting Tx: %v", txSplit)

			for split, txCount := range txSplit {
				fmt.Printf("\n")
				log.Printf("Generating %v tx with %v outputs each", txCount, split)
				for i := 0; i < txCount; i++ {
					select {
					case addr := <-com.upstream:
						select {
						case com.downstream <- TxSplit{address: addr, split: split}:
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
			}
			wg.Wait()
			fmt.Printf("\n")
			// mine the above tx in the next block
			if err := miner.StartMining(); err != nil {
				safeClose(com.exit)
				return
			}
		case <-com.exit:
			return
		}
	}
}

// Shutdown shuts down the simulation by killing the mining and the
// initial btcd processes and shuts down all actors.
func (com *Communication) Shutdown(miner *Miner, actors []*Actor, btcd *exec.Cmd) {
	defer com.wg.Done()

	<-com.exit
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
