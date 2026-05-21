package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// WriteTo emits the registry contents in Prometheus text exposition format 0.0.4.
// Each collector contributes one HELP line, one TYPE line, then its sample
// line(s). Order is stable within each collector family (counters, counter
// vecs, gauges, gauge funcs, histograms) and within each vec's label slots.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	bw := bufio.NewWriter(w)
	cw := &countingWriter{w: bw}

	r.mu.RLock()
	counters := append([]*Counter(nil), r.counters...)
	counterVecs := append([]*CounterVec(nil), r.counterVecs...)
	gauges := append([]*Gauge(nil), r.gauges...)
	gaugeFuncs := append([]*GaugeFunc(nil), r.gaugeFuncs...)
	histograms := append([]*Histogram(nil), r.histograms...)
	r.mu.RUnlock()

	for _, c := range counters {
		if err := writeCounter(cw, c); err != nil {
			return cw.n, err
		}
	}
	for _, v := range counterVecs {
		if err := writeCounterVec(cw, v); err != nil {
			return cw.n, err
		}
	}
	for _, g := range gauges {
		if err := writeGauge(cw, g); err != nil {
			return cw.n, err
		}
	}
	for _, g := range gaugeFuncs {
		if err := writeGaugeFunc(cw, g); err != nil {
			return cw.n, err
		}
	}
	for _, h := range histograms {
		if err := writeHistogram(cw, h); err != nil {
			return cw.n, err
		}
	}
	if err := bw.Flush(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func writeHelpType(w io.Writer, name, help, typ string) error {
	if _, err := fmt.Fprintf(w, "# HELP %s %s\n", name, escapeHelp(help)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", name, typ); err != nil {
		return err
	}
	return nil
}

func writeCounter(w io.Writer, c *Counter) error {
	if err := writeHelpType(w, c.name, c.help, "counter"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s %d\n", c.name, c.Get())
	return err
}

func writeCounterVec(w io.Writer, v *CounterVec) error {
	if err := writeHelpType(w, v.name, v.help, "counter"); err != nil {
		return err
	}
	v.mu.RLock()
	// Snapshot key+counter pairs, then release before formatting.
	type entry struct {
		key string
		val uint64
	}
	entries := make([]entry, 0, len(v.values))
	for k, c := range v.values {
		entries = append(entries, entry{k, c.Get()})
	}
	v.mu.RUnlock()
	// Deterministic output order — easier on golden tests and humans.
	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	for _, e := range entries {
		parts := strings.Split(e.key, "\x00")
		labelStr := formatLabels(v.labels, parts)
		if _, err := fmt.Fprintf(w, "%s%s %d\n", v.name, labelStr, e.val); err != nil {
			return err
		}
	}
	return nil
}

func writeGauge(w io.Writer, g *Gauge) error {
	if err := writeHelpType(w, g.name, g.help, "gauge"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s %d\n", g.name, g.Get())
	return err
}

func writeGaugeFunc(w io.Writer, g *GaugeFunc) error {
	if err := writeHelpType(w, g.name, g.help, "gauge"); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s %s\n", g.name, formatFloat(g.fn()))
	return err
}

func writeHistogram(w io.Writer, h *Histogram) error {
	if err := writeHelpType(w, h.name, h.help, "histogram"); err != nil {
		return err
	}
	buckets, counts, sum, total := h.snapshot()
	for i, b := range buckets {
		le := formatFloat(b)
		if _, err := fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", h.name, le, counts[i]); err != nil {
			return err
		}
	}
	// Implicit +Inf bucket equals total observations.
	if _, err := fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, total); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_count %d\n", h.name, total); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s_sum %s\n", h.name, formatFloat(sum)); err != nil {
		return err
	}
	return nil
}

// formatLabels returns the {k="v",k="v"} string for the supplied label/value
// slices. Returns "" (not "{}") when there are no labels.
func formatLabels(keys, vals []string) string {
	if len(keys) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(vals[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabelValue applies the Prometheus exposition-format escaping rules for
// label values: backslash, double quote, and newline. Other characters pass
// through unchanged (UTF-8 is allowed).
func escapeLabelValue(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// escapeHelp escapes backslash and newline in HELP text. Double quotes do NOT
// need escaping in HELP (it is not enclosed in quotes), per the spec.
func escapeHelp(s string) string {
	if !strings.ContainsAny(s, "\\\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, c := range s {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// formatFloat renders a float in the Prometheus-preferred minimal form.
// Integers stay integers, NaN/Inf use the literal tokens the spec defines.
func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
