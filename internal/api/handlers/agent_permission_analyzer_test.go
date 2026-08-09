package handlers

import "testing"

func TestDecodeAgentAnalysisJSONAcceptsFencedObject(t *testing.T) {
	t.Parallel()
	var analysis agentAIAnalysis
	err := decodeAgentAnalysisJSON("```json\n{\"goal\":\"Answer sales questions\",\"summary\":\"Reads CRM records\",\"nodes\":[],\"warnings\":[]}\n```", &analysis)
	if err != nil {
		t.Fatalf("decodeAgentAnalysisJSON: %v", err)
	}
	if analysis.Goal != "Answer sales questions" {
		t.Fatalf("goal = %q", analysis.Goal)
	}
}

func TestDecodeAgentAnalysisJSONRequiresAIGeneratedGoal(t *testing.T) {
	t.Parallel()
	var analysis agentAIAnalysis
	if err := decodeAgentAnalysisJSON(`{"summary":"No goal","nodes":[]}`, &analysis); err == nil {
		t.Fatal("analysis without an inferred goal was accepted")
	}
}
