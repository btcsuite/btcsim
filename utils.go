// Copyright (c) 2014 Conformal Systems LLC.
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/conformal/btcutil"
)

// Row represents a row in the CSV file
// and holds the key and value ints
type Row struct {
	utxoCount int
	txCount   int
}

// readCSV reads the given filename and
// returns a slice of rows
func readCSV(r io.Reader) (map[int32]*Row, error) {
	m := make(map[int32]*Row)
	reader := csv.NewReader(r)
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

func getAppDir() (string, error) {
	dir := btcutil.AppDataDir("btcsim", false)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			if err := os.Mkdir(dir, 0700); err != nil {
				return dir, err
			}
		} else {
			return dir, err
		}
	}
	return dir, nil
}

func getLogFile(prefix string) (*os.File, error) {
	var logFile *os.File
	dir, err := getAppDir()
	if err != nil {
		return logFile, err
	}
	logFile, err = os.Create(filepath.Join(dir, fmt.Sprintf("%s.log", prefix)))
	if err != nil {
		return logFile, err
	}
	return logFile, nil
}
