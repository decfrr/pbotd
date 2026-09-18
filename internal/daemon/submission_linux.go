package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/decfrr/pbotd/internal/executor"
	"github.com/decfrr/pbotd/internal/fsutil"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/model"
	"github.com/decfrr/pbotd/internal/pbs"
	"github.com/decfrr/pbotd/internal/store"
)

func (d *Daemon) validateCapacity(r model.Resources) error {
	if r.CPUs > d.capacity.CPUs || r.Memory > d.capacity.Memory {
		return fmt.Errorf("request exceeds configured CPU/memory capacity")
	}
	if d.inventory.Known && r.GPUs > len(d.inventory.Devices) {
		return fmt.Errorf("request exceeds configured GPU capacity (%d)", len(d.inventory.Devices))
	}
	return nil
}

func (d *Daemon) submit(sub *ipc.Submission) (string, error) {
	if sub == nil {
		return "", fmt.Errorf("missing submission")
	}
	if len(sub.Script) > 4<<20 || strings.ContainsRune(sub.Script, 0) {
		return "", fmt.Errorf("script must be at most 4 MiB and contain no NUL bytes")
	}
	if !filepath.IsAbs(sub.CWD) {
		return "", fmt.Errorf("submission directory must be absolute")
	}
	info, err := os.Stat(sub.CWD)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("submission working directory is not a directory")
	}
	options, err := pbs.Directives(sub.Script)
	if err != nil {
		return "", err
	}
	cli, operands, err := pbs.Parse(sub.Args, "NqloejVvJWh")
	if err != nil {
		return "", err
	}
	if len(operands) > 0 {
		return "", fmt.Errorf("unexpected submission operands")
	}
	options.Merge(cli)
	filename := filepath.Base(sub.Filename)
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	job, err := options.Resolve(d.config.DefaultRequest, name)
	if err != nil {
		return "", err
	}
	if err := d.validateCapacity(job.Request); err != nil {
		return "", err
	}
	job.Environment, err = options.Export(sub.Environment, d.config.Execution.Shell)
	if err != nil {
		return "", err
	}
	job.CWD = sub.CWD
	job.SubmitHome = sub.Environment["HOME"]
	job.SubmitPath = sub.Environment["PATH"]
	job.SubmittedAt = time.Now().UTC()
	job.QueuedAt = job.SubmittedAt
	for i, id := range job.Dependencies {
		prior, err := d.store.Find(id)
		if err != nil {
			return "", fmt.Errorf("unknown dependency %s", id)
		}
		job.Dependencies[i] = prior.PublicID
	}
	var indices []int
	if value, ok := options.Values["J"]; ok {
		indices, job.ArrayLimit, err = pbs.Array(value)
		if err != nil {
			return "", err
		}
		job.ArrayParent = true
	}
	var createdDir string
	err = d.store.Update(func(tx *store.Tx) error {
		if err := tx.Create(&job); err != nil {
			return err
		}
		createdDir = executor.JobDir(d.paths.State, job.ID)
		if err := os.Mkdir(createdDir, 0700); err != nil {
			createdDir = ""
			return err
		}
		job.ScriptPath = filepath.Join(createdDir, "snapshot.pbs")
		if err := fsutil.AtomicWrite(job.ScriptPath, []byte(sub.Script)); err != nil {
			return err
		}
		if job.ArrayParent {
			for _, index := range indices {
				child := job
				child.ID = 0
				child.ParentID = job.ID
				child.ArrayParent = false
				child.ArrayIndex = &index
				if err := tx.Create(&child); err != nil {
					return err
				}
				child.Stdout, child.Stderr = options.OutputPaths(child, job.ID)
				if err := tx.Save(child); err != nil {
					return err
				}
			}
		} else {
			job.Stdout, job.Stderr = options.OutputPaths(job, job.ID)
		}
		return tx.Save(job)
	})
	if err != nil {
		if createdDir != "" {
			os.RemoveAll(createdDir)
		}
		return "", err
	}
	d.freshGPU = false
	d.requestProbe()
	d.point("submitted")
	return job.PublicID, nil
}
