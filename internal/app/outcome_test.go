package app

import (
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

func outcome(status, ms int) ports.Outcome {
	return ports.Outcome{Status: status, HeadersSent: true, Duration: time.Duration(ms) * time.Millisecond}
}
