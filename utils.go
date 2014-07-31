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
	k int
	v int
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
