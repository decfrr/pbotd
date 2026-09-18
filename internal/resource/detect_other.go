//go:build !linux

package resource

import (
	"fmt"
	"github.com/decfrr/pbotd/internal/model"
)

func Detect() (model.Capacity, error) {
	return model.Capacity{}, fmt.Errorf("pbotd execution requires Linux or WSL")
}
