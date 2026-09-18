package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/decfrr/pbotd/internal/config"
	"github.com/decfrr/pbotd/internal/ipc"
	"github.com/decfrr/pbotd/internal/pbs"
)

const Version = "0.1.0-dev"

var help = map[string]string{
	"pbotd":    "pbotd daemon [--foreground]\npbotd status [--json]\npbotd doctor [--json]\npbotd <PBS-command> [arguments]\nPBS commands: qsub qstat qdel qhold qrls qalter qrerun pbsnodes\n",
	"qsub":     "qsub [-N name] [-q cpu|gpu] [-l resources] [-o path] [-e path]\n     [-j oe|eo|n] [-V] [-v KEY=value,OTHER] [-J ranges[%limit]]\n     [-W depend=afterok:job-id[:job-id...]] [-h] file.pbs\n",
	"qstat":    "qstat [-f] [-x] [--json] [job-id...]\n-f: details and execution history; -x: include finished jobs\n",
	"qdel":     "qdel job-id...\nCancel queued work or request termination and cleanup.\n",
	"qhold":    "qhold job-id...\nHold queued jobs.\n",
	"qrls":     "qrls job-id...\nRelease user holds.\n",
	"qalter":   "qalter [-N name] [-l resources] job-id...\nAlter queued/held jobs only.\n",
	"qrerun":   "qrerun job-id...\nRestart running individual jobs/subjobs; terminal jobs must be resubmitted.\n",
	"pbsnodes": "pbsnodes -a [--json]\nShow local capacity, reservations, and GPU availability.\n",
}

func Run(argv0 string, args []string, stdout, stderr io.Writer) int {
	command := filepath.Base(argv0)
	if _, alias := help[command]; !alias || command == "pbotd" {
		if len(args) == 0 {
			fmt.Fprint(stdout, help["pbotd"])
			return 0
		}
		command, args = args[0], args[1:]
	}
	if command == "--version" || command == "version" {
		fmt.Fprintln(stdout, "pbotd", Version)
		return 0
	}
	if command == "--help" || command == "help" {
		fmt.Fprint(stdout, help["pbotd"])
		return 0
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-?") {
		if text, ok := help[command]; ok {
			fmt.Fprint(stdout, text)
			return 0
		}
	}
	if command == "runner" || command == "workload" {
		return internalCommand(command, args, stderr)
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	c, err := config.Load(paths.Config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if command == "daemon" {
		if len(args) > 1 || len(args) == 1 && args[0] != "--foreground" {
			fmt.Fprintln(stderr, "usage: pbotd daemon [--foreground]")
			return 2
		}
		if err := runDaemon(c, paths); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if command == "doctor" {
		return doctor(c, paths, args, stdout, stderr)
	}
	request, jsonOutput, err := requestFor(command, args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := ipc.Call(ctx, paths.Socket, request)
	if err != nil && c.Daemon.Autostart && command != "status" && (errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)) {
		if startErr := autostart(ctx, paths); startErr != nil {
			fmt.Fprintln(stderr, startErr)
			return 1
		}
		response, err = ipc.Call(ctx, paths.Socket, request)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(response); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	} else {
		printResponse(command, request, response, stdout)
	}
	if response.Error != "" {
		fmt.Fprintln(stderr, response.Error)
		if response.Code == 2 {
			return 2
		}
		return 1
	}
	return 0
}

func requestFor(command string, args []string) (ipc.Request, bool, error) {
	request := ipc.Request{}
	jsonOutput := false
	switch command {
	case "qsub":
		o, operands, err := pbs.Parse(args, "NqloejVvJWh")
		if err != nil {
			return request, false, err
		}
		if len(operands) != 1 {
			return request, false, fmt.Errorf("qsub requires exactly one script file")
		}
		info, err := os.Stat(operands[0])
		if err != nil {
			return request, false, err
		}
		if info.Size() > 4<<20 {
			return request, false, fmt.Errorf("script exceeds 4 MiB")
		}
		body, err := os.ReadFile(operands[0])
		if err != nil {
			return request, false, err
		}
		if !utf8.Valid(body) {
			return request, false, fmt.Errorf("script must be UTF-8")
		}
		if _, err := pbs.Directives(string(body)); err != nil {
			return request, false, err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return request, false, err
		}
		env := make(map[string]string)
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				env[key] = value
			}
		}
		request.Operation = "submit"
		request.Submission = &ipc.Submission{Script: string(body), Filename: operands[0], CWD: cwd, Environment: env, Args: o.Arguments()}
	case "qalter":
		o, operands, err := pbs.Parse(args, "Nl")
		if err != nil {
			return request, false, err
		}
		if len(operands) == 0 || len(o.Values) == 0 {
			return request, false, fmt.Errorf("qalter requires options and job IDs")
		}
		request.Operation = "alter"
		request.IDs = operands
		request.Args = o.Arguments()
	case "qdel", "qhold", "qrls", "qrerun":
		if len(args) == 0 {
			return request, false, fmt.Errorf("%s requires job IDs", command)
		}
		request.Operation = map[string]string{"qdel": "delete", "qhold": "hold", "qrls": "release", "qrerun": "rerun"}[command]
		request.IDs = args
	case "qstat", "status", "pbsnodes":
		request.Operation = map[string]string{"qstat": "jobs", "status": "status", "pbsnodes": "nodes"}[command]
		allNodes := false
		for _, arg := range args {
			switch {
			case arg == "--json":
				jsonOutput = true
			case arg == "-a" && command == "pbsnodes":
				allNodes = true
			case arg == "-f" && command == "qstat":
				request.Full = true
			case arg == "-x" && command == "qstat":
				request.All = true
			case !strings.HasPrefix(arg, "-") && command == "qstat":
				request.IDs = append(request.IDs, arg)
			default:
				return request, false, fmt.Errorf("unsupported %s argument %q", command, arg)
			}
		}
		if command == "pbsnodes" && !allNodes {
			return request, false, fmt.Errorf("pbsnodes requires -a")
		}
	default:
		return request, false, fmt.Errorf("unknown command %q; use pbotd --help", command)
	}
	return request, jsonOutput, nil
}

func printResponse(command string, request ipc.Request, response ipc.Response, out io.Writer) {
	if command == "qsub" {
		if response.JobID != "" {
			fmt.Fprintln(out, response.JobID)
		}
		return
	}
	if response.Node != nil {
		n := response.Node
		fmt.Fprintf(out, "%s\n  CPUs: %d reserved / %d total\n  Memory: %d reserved / %d total bytes\n", n.Hostname, n.Reserved.CPUs, n.Capacity.CPUs, n.Reserved.Memory, n.Capacity.Memory)
		fmt.Fprintf(out, "  Free: %d CPUs, %d memory bytes\n", max(0, n.Capacity.CPUs-n.Reserved.CPUs), max(int64(0), n.Capacity.Memory-n.Reserved.Memory))
		counts, _ := json.Marshal(n.Counts)
		fmt.Fprintf(out, "  Jobs: %s\n  GPUs: %d reserved / %d discovered\n", counts, n.Reserved.GPUs, len(n.Inventory.Devices))
		for _, device := range n.Inventory.Devices {
			fmt.Fprintf(out, "  GPU %s available=%t model=%q reason=%q allocated_to=%q\n", device.UUID, device.Available, device.Model, device.Reason, device.AllocatedTo)
		}
		if n.Inventory.Error != "" {
			fmt.Fprintf(out, "  GPU probe: %s\n", n.Inventory.Error)
		}
		return
	}
	if command != "qstat" {
		return
	}
	if request.Full {
		for _, j := range response.Jobs {
			fmt.Fprintf(out, "Job Id: %s\n    Job_Name = %s\n    job_state = %s\n    state = %s\n    queue = %s\n    reason = %q\n    Resource_List.ncpus = %d\n    Resource_List.mem = %d\n    Resource_List.ngpus = %d\n    Resource_List.walltime = %d\n    attempt = %s\n    GPU_UUIDs = %s\n    Output_Path = %q\n    Error_Path = %q\n", j.PublicID, j.Name, j.State.Code(), j.State, j.Queue, j.Reason, j.Request.CPUs, j.Request.Memory, j.Request.GPUs, j.Request.Walltime, j.AttemptID, strings.Join(j.GPUs, ","), j.Stdout, j.Stderr)
			if j.ExitCode != nil {
				fmt.Fprintf(out, "    Exit_status = %d\n", *j.ExitCode)
			}
			if j.Signal != 0 {
				fmt.Fprintf(out, "    term_signal = %d\n", j.Signal)
			}
			if j.ArrayParent {
				counts, _ := json.Marshal(j.StateCounts)
				fmt.Fprintf(out, "    array_state_counts = %s\n", counts)
			}
			fmt.Fprintf(out, "    submit_time = %s\n", j.SubmittedAt.Format(time.RFC3339Nano))
			if j.StartedAt != nil {
				fmt.Fprintf(out, "    start_time = %s\n", j.StartedAt.Format(time.RFC3339Nano))
			}
			if j.EndedAt != nil {
				fmt.Fprintf(out, "    end_time = %s\n", j.EndedAt.Format(time.RFC3339Nano))
			}
			for _, a := range response.Attempts {
				if a.JobID == j.ID && a.Result != nil {
					fmt.Fprintf(out, "    attempt_result[%s] = %s user_cpu=%.6fs system_cpu=%.6fs max_rss=%d\n", a.ID, a.Result.State, a.Result.Usage.UserSeconds, a.Result.Usage.SystemSeconds, a.Result.Usage.MaxRSSBytes)
				}
			}
		}
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "Job ID\tName\tQueue\tS\tCPUs\tGPUs\tReason")
	for _, j := range response.Jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\n", j.PublicID, j.Name, j.Queue, j.State.Code(), j.Request.CPUs, j.Request.GPUs, j.Reason)
	}
	w.Flush()
}
