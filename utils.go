// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"encoding/csv"
	"io"
	"os"
	"strconv"
)

// Row represents a row in the CSV file
// and holds the key and value ints
type Row struct {
	utxoCount int
	txCount   int
}

// readCSV reads the given filename and
// returns a slice of rows
func readCSV(f string) (map[int32]*Row, error) {
	file, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	m := make(map[int32]*Row)
	reader := csv.NewReader(file)
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		b, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, err
		}
		u, err := strconv.Atoi(row[1])
		if err != nil {
			return nil, err
		}
		t, err := strconv.Atoi(row[2])
		if err != nil {
			return nil, err
		}
		m[int32(b)] = &Row{u, t}
	}
	return m, nil
}
