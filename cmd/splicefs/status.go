package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/BlueRival/splicefs/client/app"
)

// printStatus asks the running client for its report and prints it, as a table or as the
// JSON the client sent.
func (b backend) printStatus(ctx context.Context, inv invocation, stdout io.Writer) error {
	report, err := b.status(ctx, app.SocketPath(inv.configDir))
	if err != nil {
		return err
	}
	if inv.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return writeReport(stdout, report)
}

// writeReport prints a report as a few short tables: the mounts, then for each cluster
// its nodes with their measured speed, then the cache directories.
func writeReport(w io.Writer, r app.Report) error {
	mounts := [][]string{{"MOUNT", "STATE", "SOURCE"}}
	for _, m := range r.Mounts {
		mounts = append(mounts, []string{m.Target, m.State, m.Source})
	}
	sections := [][][]string{mounts}
	for _, c := range r.Clusters {
		sections = append(sections, clusterRows(c))
	}
	if len(r.Caches) > 0 {
		caches := [][]string{{"CACHE", "FILES", "SIZE"}}
		for _, c := range r.Caches {
			caches = append(caches, []string{c.Dir, fmt.Sprint(c.Entries), formatBytes(float64(c.Bytes))})
		}
		sections = append(sections, caches)
	}
	for i, rows := range sections {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := writeTable(w, rows); err != nil {
			return err
		}
	}
	return nil
}

func clusterRows(c app.ClusterReport) [][]string {
	rows := [][]string{{"CLUSTER " + c.Name, "RATE"}}
	if len(c.Nodes) == 0 {
		return append(rows, []string{"(no nodes known yet)"})
	}
	for _, n := range c.Nodes {
		rate := "not measured"
		if n.Measured {
			rate = fmt.Sprintf("%.1f MB/s", n.BytesPerSecond/1e6)
		}
		rows = append(rows, []string{n.Name, rate})
	}
	return rows
}

func writeTable(w io.Writer, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				_, _ = io.WriteString(tw, "\t")
			}
			_, _ = io.WriteString(tw, cell)
		}
		_, _ = io.WriteString(tw, "\n")
	}
	return tw.Flush()
}

// formatBytes renders a size in decimal units, like the rates of the nodes.
func formatBytes(n float64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for n >= 1000 && i < len(units)-1 {
		n /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", n)
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
