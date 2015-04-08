/*
 * Copyright (c) 2014-2015 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

func getLogFile(prefix string) (*os.File, error) {
	return os.Create(filepath.Join(AppDataDir, fmt.Sprintf("%s.log", prefix)))
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}
