package stdout

import (
	"encoding/json"
	"fmt"

	"github.com/xslonikx/snout/internal/logging"
)

// Consumer writes both route and peer events as JSON.
type Consumer struct{}

func (Consumer) Consume(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		logging.Errorf("marshal event: %v", err)
		return
	}

	fmt.Println(string(data))
}
