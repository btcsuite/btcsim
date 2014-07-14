package main

// place and start of file io utils for sim configurations

import (
	"encoding/csv"
	"io"
	"log"
	"os"
	"strconv"
)

// reads csv of two columns of ints per initial spec for tx curves
func ReadCSV(f string) map[int]int {
	file, err := os.Open(f)
	if err != nil {
		log.Printf("%s configuration file not loaded", err)
		// return value if needed
	}
	defer file.Close()
	reader := csv.NewReader(file)
	reader.Comma = ','
	var params map[int]int
	params = make(map[int]int)
	i := 0
	for {
		i++
		record, err := reader.Read()
		if err == io.EOF {
			break
		} else if err != nil {
			log.Printf("Config Read Error: %s", err)
		}
		k, err1 := strconv.Atoi(record[0])
		v, err2 := strconv.Atoi(record[1])
		if err1 != nil || err2 != nil {
			log.Printf("Config Ln: %d not an integer.", i)
		}
		params[k] = v
	}
	return params
}
