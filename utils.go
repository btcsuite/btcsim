// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"encoding/csv"
	"io"
	"log"
	"os"
	"strconv"
)

// Row represents a row in the CSV file
// and holds the key and value ints
type Row struct {
	k int
	v int
}

// newCSV creates a new csv file containing a transaction curve
// of <block #>, <txCount> fields
func newCSV() error {
	if _, err := os.Open(*txCurvePath); err == nil {
		// We don't want to overwrite any existing csv file
		return nil
	}

	// csv file does not exist so we can create a new one
	file, err := os.Create(*txCurvePath)
	if err != nil {
		return err
	}
	defer file.Close()

	log.Println("Creating new csv file...")

	write := csv.NewWriter(file)
	var k, block int = (*maxBlocks - *matureBlock) / 4, 0
	for i := *matureBlock + 1; i <= *maxBlocks; i++ {
		record := []string{strconv.Itoa(i), strconv.Itoa(*tpb)}
		if err := write.Write(record); err != nil {
			return err
		}
		write.Flush()
		block++

		// Quadruple tpb every k blocks
		if block%k == 0 {
			*tpb *= 4
		}
	}

	log.Println("New csv file created.")
	return nil
}

// readCSV reads the given filename and
// returns a slice of rows
func readCSV(f string) ([]*Row, error) {
	file, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := csv.NewReader(file)
	rows := make([]*Row, 0)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		k, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, err
		}
		v, err := strconv.Atoi(row[1])
		if err != nil {
			return nil, err
		}
		rows = append(rows, &Row{k, v})
	}
	return rows, nil
}
