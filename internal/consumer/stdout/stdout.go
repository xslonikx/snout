package stdout

import (
	"encoding/json"
	"fmt"
	"log"
)

// Consumer writes both route and peer events as JSON.
type Consumer struct{}

func (Consumer) Consume(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		log.Printf("marshal event: %v", err)
		return
	}

	fmt.Println(string(data))
}
