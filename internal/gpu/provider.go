package gpu

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/decfrr/pbotd/internal/config"
	"github.com/decfrr/pbotd/internal/model"
)

type Provider struct {
	config  config.Config
	command string
	last    model.Inventory
}

func New(c config.Config) *Provider {
	p := &Provider{config: c}
	if c.GPU.Command != "" {
		p.command = c.GPU.Command
		return p
	}
	for _, candidate := range []string{"nvidia-smi", "/usr/bin/nvidia-smi", "/usr/lib/wsl/lib/nvidia-smi", "/bin/nvidia-smi"} {
		if path, err := exec.LookPath(candidate); err == nil {
			p.command = path
			break
		}
	}
	return p
}

func (p *Provider) query(ctx context.Context, fields string) ([]byte, error) {
	if p.command == "" {
		return nil, fmt.Errorf("nvidia-smi not found")
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.ProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.command, fields, "--format=csv,noheader,nounits")
	cmd.WaitDelay = 100 * time.Millisecond
	body, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("GPU probe: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("GPU probe failed: %w", err)
	}
	return body, nil
}

// Refresh preserves device identities during failed discovery; unknown is never free.
// Calls are serialized by the daemon's single inventory worker.
func (p *Provider) Refresh(ctx context.Context) model.Inventory {
	inv := model.Inventory{At: time.Now().UTC()}
	if p.config.GPU.Provider == "static" {
		inv.Known = true
		for _, id := range p.config.GPU.Static {
			inv.Devices = append(inv.Devices, model.Device{UUID: id, Available: true})
		}
	} else {
		body, err := p.query(ctx, "--query-gpu=index,uuid,name,memory.total")
		if err == nil {
			inv.Devices, err = ParseInventory(string(body))
		}
		if err == nil {
			inv.Known = true
		} else {
			inv.Error = err.Error()
			if len(p.config.GPU.Static) > 0 {
				inv.Known = true
				for _, id := range p.config.GPU.Static {
					inv.Devices = append(inv.Devices, model.Device{UUID: id, Available: true})
				}
			} else {
				inv.Known = p.last.Known
				inv.Devices = append([]model.Device(nil), p.last.Devices...)
				for i := range inv.Devices {
					inv.Devices[i].Available = false
					inv.Devices[i].Reason = "GPUUnavailable"
				}
			}
		}
	}
	filtered := make([]model.Device, 0, len(inv.Devices))
	for _, d := range inv.Devices {
		if slices.Contains(p.config.GPU.Deny, d.UUID) || len(p.config.GPU.Allow) > 0 && !slices.Contains(p.config.GPU.Allow, d.UUID) {
			continue
		}
		filtered = append(filtered, d)
	}
	inv.Devices = filtered
	if p.config.GPU.ForeignPolicy == "avoid" && len(inv.Devices) > 0 {
		body, err := p.query(ctx, "--query-compute-apps=gpu_uuid,pid")
		var processes map[string][]int
		if err == nil {
			processes, err = ParseProcesses(string(body))
		}
		if err != nil {
			inv.Error = err.Error()
			for i := range inv.Devices {
				inv.Devices[i].Available = false
				inv.Devices[i].Reason = "GPUOccupancyUnknown"
			}
		} else {
			for i := range inv.Devices {
				inv.Devices[i].Processes = processes[inv.Devices[i].UUID]
			}
		}
	}
	slices.SortFunc(inv.Devices, func(a, b model.Device) int { return strings.Compare(a.UUID, b.UUID) })
	p.last = inv
	return inv
}

func ParseInventory(body string) ([]model.Device, error) {
	r := csv.NewReader(strings.NewReader(body))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 4
	var devices []model.Device
	seen := make(map[string]bool)
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("GPU inventory CSV: %w", err)
		}
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
		index, err := strconv.Atoi(row[0])
		if err != nil || index < 0 {
			return nil, fmt.Errorf("invalid GPU index %q", row[0])
		}
		if !config.ValidUUID(row[1]) || seen[row[1]] {
			return nil, fmt.Errorf("invalid/duplicate physical GPU UUID %q", row[1])
		}
		seen[row[1]] = true
		mem, err := strconv.ParseInt(row[3], 10, 64)
		if err != nil || mem < 0 || mem > (1<<63-1)>>20 {
			return nil, fmt.Errorf("invalid GPU memory %q", row[3])
		}
		devices = append(devices, model.Device{Index: &index, UUID: row[1], Model: row[2], Memory: mem << 20, Available: true})
	}
	return devices, nil
}

func ParseProcesses(body string) (map[string][]int, error) {
	r := csv.NewReader(strings.NewReader(body))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 2
	processes := make(map[string][]int)
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("GPU process CSV: %w", err)
		}
		id := strings.TrimSpace(row[0])
		pid, err := strconv.Atoi(strings.TrimSpace(row[1]))
		if !config.ValidUUID(id) || err != nil || pid <= 0 {
			return nil, fmt.Errorf("unclassifiable GPU process record")
		}
		processes[id] = append(processes[id], pid)
	}
	return processes, nil
}
