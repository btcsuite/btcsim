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
	downstream   chan btcutil.Address
	timeReceived chan time.Time
	blockTxCount chan int
	exit         chan struct{}
	errChan      chan struct{}
	height       chan int32
	split        chan int
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
		downstream:   make(chan btcutil.Address, *maxActors),
		timeReceived: make(chan time.Time, *maxActors),
		blockTxCount: make(chan int, *maxActors),
		height:       make(chan int32),
		split:        make(chan int),
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
func (com *Communication) Start(actors []*Actor, client *rpc.Client,
	btcd *exec.Cmd, txCurve map[int32]*Row) (tpsChan chan float64, tpbChan chan int) {
	tpsChan = make(chan float64, 1)
	tpbChan = make(chan int, 1)

	// Start actors
	for _, a := range actors {
		com.wg.Add(1)
		go func(a *Actor, com *Communication) {
			defer com.wg.Done()
			if err := a.Start(os.Stderr, os.Stdout, com); err != nil {
				log.Printf("Cannot start actor on %s: %v", "localhost:"+a.args.port, err)
				a.Shutdown()
			}
		}(a, com)
	}

	// Start a goroutine to check if all actors have failed
	com.wg.Add(1)
	go com.failedActors()

	miningAddrs := make([]btcutil.Address, *maxActors)
	for i, a := range actors {
		select {
		case miningAddrs[i] = <-a.miningAddr:
		case <-a.quit:
			// This actor has quit
			select {
			case <-com.exit:
				close(tpsChan)
				close(tpbChan)
				return
			default:
			}
		}
	}

	// Start mining.
	miner, err := NewMiner(miningAddrs, com.exit, com.height, com.txpool)
	if err != nil {
		safeClose(com.exit) // make failedActors goroutine exit
		close(tpsChan)
		close(tpbChan)
		com.wg.Add(1)
		go com.Shutdown(miner, actors, btcd)
		return
	}

	// Add mining btcd listen interface as a node
	client.AddNode("localhost:18550", rpc.ANAdd)

	// Start a goroutine to estimate tps
	com.wg.Add(1)
	go com.estimateTps(tpsChan, txCurve)

	// Start a goroutine to find max tpb
	com.wg.Add(1)
	go com.estimateTpb(tpbChan)

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
						case actor.utxoQueue.coinbaseQueue <- tx:
							break next
						default:
							// dequeue the first mature tx
							mTx := <-actor.utxoQueue.coinbaseQueue
							// enqueue the latest tx
							actor.utxoQueue.coinbaseQueue <- tx
							// we'll process the mature tx next
							// so point tx to mTx
							tx = mTx
							// reset vout as per the new tx
							vout = tx.MsgTx().TxOut[n]
						}
					}
					txout := com.getUtxo(tx, vout, uint32(n))
					// add utxo to actor's pool
					actor.utxoQueue.enqueueUtxo <- txout
				}
			}
			// allow Communicate to sync with the processed block
			if b.height >= int32(*matureBlock)-1 {
				var txCount, utxoCount int
				for _, a := range actors {
					utxoCount += len(a.utxoQueue.utxos)
				}
				txCount = len(block.Transactions())
				log.Printf("%v: block# %v: no of utxos: %v, no of transactions: %v", b.height,
					b.hash, utxoCount, txCount)
				select {
				case com.processed <- b:
				case <-com.exit:
					return
				}
				select {
				case com.blockTxCount <- txCount:
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
		for _, actorAddr := range actor.ownedAddresses {
			if addr == actorAddr.String() {
				return actor, nil
			}
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

// estimateTpb sends the maximum transactions over the returned chan
func (com *Communication) estimateTpb(tpbChan chan<- int) {
	defer com.wg.Done()

	var maxTpb int

	for {
		select {
		case last := <-com.blockTxCount:
			if last > maxTpb {
				maxTpb = last
			}
		case <-com.exit:
			tpbChan <- maxTpb
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
				utxoCount += len(a.utxoQueue.utxos)
			}

			// the required transactions are divided into two groups because we need some of them to
			// contribute to the utxo count required for the next block and the rest to contribute to
			// the tx count
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
			// we need to create 40K-19899 = 20101 utxos so in this case, so
			// we create 20101 tx which give 1 net utxo output
			//
			// at block 20000, we need to ensure that next block has 50K utxos
			// we already have 40K by the previous iteration, so we need 50-40 = 10K utxos
			// we also need to generate 20K tx before the next block, so
			// create 10000 tx which generate 1 net utxo plus 10000 tx without any net utxo
			//
			// since we cannot generate more tx than the no of available utxos, the no of tx
			// that can be generated at any iteration is limited by the utxos available

			// in case the next row doesn't exist, we initialize the required no of utxos to zero
			// so we keep the utxoCount same as current count
			next, ok := txCurve[h+1]
			if !ok {
				next = &Row{}
				next.utxoCount = utxoCount
			}

			// reqUtxoCount is the number of utxos required
			reqUtxoCount := 0
			if next.utxoCount > utxoCount {
				reqUtxoCount = next.utxoCount - utxoCount
			}

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
				log.Printf("Warning: capping no of transactions at %v based on no of available utxos", utxoCount)
				// cap the total no of tx at the no of available utxos
				reqTxCount = utxoCount
			}

			var multiplier, totalUtxos, totalTx int
			// skip if we already have more than the no of utxos required
			if reqUtxoCount > 0 {
				// e.g: if we need 18K utxos in 12K tx
				// multiplier = [18000/12000] = [1.5] = 2
				// totalUtxos = 18000/2 = 9000
				// totalTx = 120000 - 9000 = 3000
				multiplier = int(math.Ceil(float64(reqUtxoCount) / float64(reqTxCount)))
				totalUtxos = reqUtxoCount / multiplier
			}

			// if we're not already covered by the utxo transactions, generate additional tx
			if reqTxCount > totalUtxos {
				totalTx = reqTxCount - totalUtxos
			}

			log.Printf("=== row: %v === next block utxos: %v === transactions: %v", h, next.utxoCount, row.txCount)

			if totalTx > 0 {
				log.Printf("Generating %v transactions ...", totalTx)
				for i := 0; i < totalTx; i++ {
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
			}

			if totalUtxos > 0 {
				fmt.Printf("\n")
				log.Printf("Generating %v utxos by creating %v transactions with %v net outputs each ...", reqUtxoCount, totalUtxos, multiplier)
				for i := 0; i < totalUtxos; i++ {
					select {
					case com.split <- multiplier:
						// For every address sent downstream (one transaction about to happen),
						// spawn a goroutine to listen for an accepted transaction in the mempool
						wg.Add(1)
						go com.txPoolRecv(&wg)
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
	for _, a := range actors {
		a.Shutdown()
	}
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
