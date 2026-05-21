package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestRegistry_Counter_Increments(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("ephemera_test_counter", "test counter")
	if got := c.Get(); got != 0 {
		t.Fatalf("expected initial 0, got %d", got)
	}
	c.Inc()
	c.Add(5)
	if got := c.Get(); got != 6 {
		t.Fatalf("expected 6 after Inc+Add(5), got %d", got)
	}
}

func TestRegistry_CounterVec_LabelsSeparate(t *testing.T) {
	r := NewRegistry()
	v := r.NewCounterVec("ephemera_test_vec", "test vec", "outcome")
	v.WithLabelValues("ok").Inc()
	v.WithLabelValues("ok").Inc()
	v.WithLabelValues("fail").Inc()

	if got := v.WithLabelValues("ok").Get(); got != 2 {
		t.Errorf("expected ok=2, got %d", got)
	}
	if got := v.WithLabelValues("fail").Get(); got != 1 {
		t.Errorf("expected fail=1, got %d", got)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on label count mismatch")
		}
	}()
	v.WithLabelValues("ok", "extra")
}

func TestRegistry_Gauge_SetReturnsLastValue(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("ephemera_test_gauge", "test gauge")
	g.Set(42)
	g.Inc()
	g.Add(-3)
	if got := g.Get(); got != 40 {
		t.Fatalf("expected 40 (42+1-3), got %d", got)
	}
	g.Dec()
	if got := g.Get(); got != 39 {
		t.Fatalf("expected 39 after Dec, got %d", got)
	}
}

func TestRegistry_GaugeFunc_CallsFunctionEachWrite(t *testing.T) {
	r := NewRegistry()
	var calls int
	r.NewGaugeFunc("ephemera_test_gaugefunc", "test gauge func", func() float64 {
		calls++
		return float64(calls * 10)
	})

	var buf bytes.Buffer
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatalf("first WriteTo: %v", err)
	}
	first := buf.String()
	buf.Reset()
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatalf("second WriteTo: %v", err)
	}
	second := buf.String()

	if !strings.Contains(first, "ephemera_test_gaugefunc 10") {
		t.Errorf("first call should return 10:\n%s", first)
	}
	if !strings.Contains(second, "ephemera_test_gaugefunc 20") {
		t.Errorf("second call should return 20:\n%s", second)
	}
}

func TestRegistry_Histogram_BucketsCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("ephemera_test_hist", "test hist", []float64{0.1, 1, 10})
	for _, v := range []float64{0.05, 0.3, 1.5, 7, 15} {
		h.Observe(v)
	}
	var buf bytes.Buffer
	if _, err := r.WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		`ephemera_test_hist_bucket{le="0.1"} 1`,
		`ephemera_test_hist_bucket{le="1"} 2`,
		`ephemera_test_hist_bucket{le="10"} 4`,
		`ephemera_test_hist_bucket{le="+Inf"} 5`,
		`ephemera_test_hist_count 5`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing line %q in:\n%s", want, out)
		}
	}
	// sum = 0.05 + 0.3 + 1.5 + 7 + 15 = 23.85
	if !strings.Contains(out, "ephemera_test_hist_sum 23.85") {
		t.Errorf("expected sum 23.85, got:\n%s", out)
	}
}

func TestRegistry_WriteTo_FormatMatchesSpec(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("ephemera_simple_total", "Simple total counter.")
	c.Add(42)
	g := r.NewGauge("ephemera_simple_count", "Simple gauge.")
	g.Set(7)

	var buf bytes.Buffer
	r.WriteTo(&buf)
	want := "# HELP ephemera_simple_total Simple total counter.\n" +
		"# TYPE ephemera_simple_total counter\n" +
		"ephemera_simple_total 42\n" +
		"# HELP ephemera_simple_count Simple gauge.\n" +
		"# TYPE ephemera_simple_count gauge\n" +
		"ephemera_simple_count 7\n"
	if got := buf.String(); got != want {
		t.Errorf("exposition format mismatch.\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestRegistry_WriteTo_EscapesLabelValues(t *testing.T) {
	r := NewRegistry()
	v := r.NewCounterVec("ephemera_escape_total", "Escape test", "tag")
	v.WithLabelValues(`weird"value`).Inc()
	v.WithLabelValues("back\\slash").Inc()
	v.WithLabelValues("multi\nline").Inc()

	var buf bytes.Buffer
	r.WriteTo(&buf)
	out := buf.String()

	for _, want := range []string{
		`ephemera_escape_total{tag="back\\slash"} 1`,
		`ephemera_escape_total{tag="multi\nline"} 1`,
		`ephemera_escape_total{tag="weird\"value"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing escaped line %q in:\n%s", want, out)
		}
	}
}

func TestRegistry_Concurrent_NoRace(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("ephemera_race_counter", "race")
	v := r.NewCounterVec("ephemera_race_vec", "race", "label")
	g := r.NewGauge("ephemera_race_gauge", "race")
	h := r.NewHistogram("ephemera_race_hist", "race", nil)

	const goroutines = 16
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				c.Inc()
				v.WithLabelValues("hot").Inc()
				g.Add(1)
				g.Add(-1)
				h.Observe(float64(j % 10))
			}
		}(i)
	}

	// Concurrently scrape to validate WriteTo is read-safe while writers are
	// active. The scrape result is discarded; we only care about race detection.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				var b bytes.Buffer
				r.WriteTo(&b)
			}
		}()
	}

	wg.Wait()

	if got := c.Get(); got != uint64(goroutines*iters) {
		t.Errorf("counter expected %d, got %d", goroutines*iters, got)
	}
	if got := v.WithLabelValues("hot").Get(); got != uint64(goroutines*iters) {
		t.Errorf("counter vec expected %d, got %d", goroutines*iters, got)
	}
	if got := g.Get(); got != 0 {
		t.Errorf("gauge expected 0, got %d", got)
	}
}

func TestRegistry_DuplicateName_Panics(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("dup_metric", "first")
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on duplicate registration")
		}
	}()
	r.NewGauge("dup_metric", "second")
}

func TestRegistry_InvalidName_Panics(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on invalid name")
		}
	}()
	r.NewCounter("123_starts_with_digit", "bad")
}

func TestFormatFloat_IntegralVsFractional(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{-7, "-7"},
		{0.5, "0.5"},
		{23.85, "23.85"},
		{1e20, "1e+20"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v): want %q, got %q", c.in, c.want, got)
		}
	}
}
