package main

import (
	"log"
	"os"
)

// txInhibitLine drives a gpio-sim line's simulated input through its pull
// attribute: "pull-up" (direwolf reads 1, inhibited) or "pull-down".
func txInhibitLine(path string) func(on bool) {
	failed := false
	return func(on bool) {
		v := "pull-down\n"
		if on {
			v = "pull-up\n"
		}
		if err := os.WriteFile(path, []byte(v), 0); err != nil {
			if !failed {
				log.Printf("-txinh: %v", err)
				failed = true
			}
			return
		}
		if failed {
			log.Printf("-txinh: %s writable again", path)
			failed = false
		}
	}
}
