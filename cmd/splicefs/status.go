package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/SpliceFS/client/app"
)

const (
	// tableColumnGap is the number of spaces between the columns of a status table.
	tableColumnGap = 2
	// bytesPerMegabyte converts the measured speed of a node to the unit shown, decimal like
	// formatBytes.
	bytesPerMegabyte = 1e6
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
		//nolint:wrapcheck // The error of the write already names the file, and the command prints it as it is.
		return enc.Encode(report)
	}
	return writeReport(stdout, report)
}

// writeReport prints a report as a few short tables: the mounts, then for each cluster
// its nodes with their measured speed, then the cache directories.
func writeReport(w io.Writer, r app.Report) error {
	tables := [][][]string{mountRows(r.Mounts)}
	for _, c := range r.Clusters {
		tables = append(tables, clusterRows(c))
	}
	if len(r.Caches) > 0 {
		tables = append(tables, cacheRows(r.Caches))
	}
	return writeTables(w, tables)
}

func mountRows(mounts []app.MountReport) [][]string {
	rows := make([][]string, 0, 1+len(mounts))
	rows = append(rows, []string{"MOUNT", "STATE", "SOURCE"})
	for _, m := range mounts {
		rows = append(rows, []string{m.Target, m.State, m.Source})
	}
	return rows
}

func clusterRows(c app.ClusterReport) [][]string {
	rows := [][]string{{"CLUSTER " + c.Name, "RATE"}}
	if len(c.Nodes) == 0 {
		return append(rows, []string{"(no nodes known yet)"})
	}
	for _, n := range c.Nodes {
		rate := "not measured"
		if n.Measured {
			rate = fmt.Sprintf("%.1f MB/s", n.BytesPerSecond/bytesPerMegabyte)
		}
		rows = append(rows, []string{n.Name, rate})
	}
	return rows
}

func cacheRows(caches []app.CacheReport) [][]string {
	rows := make([][]string, 0, 1+len(caches))
	rows = append(rows, []string{"CACHE", "FILES", "SIZE"})
	for _, c := range caches {
		rows = append(rows, []string{c.Dir, strconv.Itoa(c.Entries), formatBytes(float64(c.Bytes))})
	}
	return rows
}

// tableText joins the cells of each row with tabs, for the table writer to align.
func tableText(rows [][]string) string {
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = strings.Join(row, "\t") + "\n"
	}
	return strings.Join(lines, "")
}

// writeTables prints the tables with a blank line between them. One writer serves all of
// them: a blank line ends the columns of the table above it, so each is aligned on its own.
func writeTables(w io.Writer, tables [][][]string) error {
	texts := make([]string, len(tables))
	for i, rows := range tables {
		texts[i] = tableText(rows)
	}
	tw := tabwriter.NewWriter(w, 0, 0, tableColumnGap, ' ', 0)
	// The errors of the writes already name the file, and the command prints them as they are.
	if _, err := io.WriteString(tw, strings.Join(texts, "\n")); err != nil {
		return err //nolint:wrapcheck // See above.
	}
	return tw.Flush() //nolint:wrapcheck // See above.
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
