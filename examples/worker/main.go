// worker is a minimal stdlib-only Go background process: it ticks every 5s
// and touches /tmp/heartbeat so an exec health probe can observe liveness.
// Used as a qooq example with no exposed port and policy.restart: always.
package main

import (
	"log"
	"os"
	"time"
)

const heartbeatPath = "/tmp/heartbeat"

func main() {
	for {
		log.Println("tick")
		if err := touch(heartbeatPath); err != nil {
			log.Printf("heartbeat write failed: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func touch(path string) error {
	now := time.Now()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, ferr := os.Create(path)
		if ferr != nil {
			return ferr
		}
		return f.Close()
	}
	return os.Chtimes(path, now, now)
}
