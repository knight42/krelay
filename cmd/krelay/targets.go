package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// targetSpec is one forward target as given on the command line or in a
// targets file: a resource, its port mappings, and per-target overrides.
type targetSpec struct {
	resource   string
	ports      []string
	namespace  string
	listenAddr string
}

// parseTargetsFile reads one target per line, using the same syntax as the
// command line. Empty lines and lines starting with '#' or '//' are skipped.
// A line may override the namespace with "-n NS" and the listen address with
// "-l ADDR" before the resource.
func parseTargetsFile(path, defaultNamespace, defaultListenAddr string) ([]targetSpec, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}

	var specs []targetSpec
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
			continue
		}
		spec, err := parseTargetLine(line, defaultNamespace, defaultListenAddr)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		specs = append(specs, spec)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("no targets found in %s", path)
	}
	return specs, nil
}

func parseTargetLine(line, defaultNamespace, defaultListenAddr string) (targetSpec, error) {
	spec := targetSpec{
		namespace:  defaultNamespace,
		listenAddr: defaultListenAddr,
	}
	fields := strings.Fields(line)
	for i := 0; i < len(fields); i++ {
		switch field := fields[i]; field {
		case "-n", "--namespace":
			if i+1 >= len(fields) {
				return spec, fmt.Errorf("missing value for %s", field)
			}
			i++
			spec.namespace = fields[i]
		case "-l", "--address":
			if i+1 >= len(fields) {
				return spec, fmt.Errorf("missing value for %s", field)
			}
			i++
			spec.listenAddr = fields[i]
		default:
			if spec.resource == "" {
				spec.resource = field
			} else {
				spec.ports = append(spec.ports, field)
			}
		}
	}
	if spec.resource == "" || len(spec.ports) == 0 {
		return spec, fmt.Errorf("expected TYPE/NAME followed by ports, got %q", line)
	}
	return spec, nil
}
