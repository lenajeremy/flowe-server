package executor

import "testing"

func TestSubstituteTemplatesFieldPath(t *testing.T) {
	outputs := map[string]string{
		"wh-1":  `{"id":"23634","filename":"Tickets.pdf","meta":{"count":10}}`,
		"llm-1": "plain text output",
	}
	cases := map[string]string{
		"{{wh-1.output}}":            `{"id":"23634","filename":"Tickets.pdf","meta":{"count":10}}`,
		"{{wh-1.output.id}}":         "23634",
		"{{wh-1.output.filename}}":   "Tickets.pdf",
		"{{wh-1.output.meta.count}}": "10",
		"{{llm-1.output}}":           "plain text output",
		"{{missing.output}}":         "[no output from missing]",
		"{{wh-1.output.nope}}":       "[no field nope]",
		"{{llm-1.output.x}}":         "[x unavailable — output is not JSON]",
	}
	for in, want := range cases {
		if got := substituteTemplates(in, outputs); got != want {
			t.Errorf("%s => %q, want %q", in, got, want)
		}
	}

	// Mixed inline text with two field tokens resolves both.
	inline := "from {{wh-1.output.filename}} count {{wh-1.output.meta.count}}"
	if got := substituteTemplates(inline, outputs); got != "from Tickets.pdf count 10" {
		t.Errorf("inline => %q", got)
	}
}
