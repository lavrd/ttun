package main

import (
	"crypto/rand"
	"os"
)

func main() {
	file, err := os.OpenFile("mock_data.txt", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer file.Close()

	const fileSize = 1 * 1024 * 1024 * 100
	const bufferSize = 1024 * 1024
	buffer := make([]byte, bufferSize)
	for i := int64(0); i < fileSize; i += bufferSize {
		if _, err = rand.Read(buffer); err != nil {
			panic(err)
		}
		if _, err = file.Write(buffer); err != nil {
			panic(err)
		}
	}
}
