package govdao

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		render string
		want   string
	}{
		{"Status: ACCEPTED", Accepted},
		{"Status: REJECTED", Rejected},
		{"Status: ACTIVE — open for votes", Pending},
		{"Vote YES / Vote NO", Pending},
		{"something entirely unexpected", Unknown},
	}
	for _, tt := range tests {
		if got := classify(tt.render); got != tt.want {
			t.Errorf("classify(%q) = %q, want %q", tt.render, got, tt.want)
		}
	}
}

func TestExtractProposalID(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "json events array",
			out:  `tx result EVENTS: [{"type":"ProposalCreated","attrs":[{"key":"id","value":"7"}]}] done`,
			want: "7",
		},
		{
			name: "text fallback",
			out:  "Event ProposalCreated fired with id 42 on gno.land/r/gov/dao",
			want: "42",
		},
		{
			name: "not present",
			out:  "some unrelated output",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractProposalID(tt.out); got != tt.want {
				t.Errorf("ExtractProposalID() = %q, want %q", got, tt.want)
			}
		})
	}
}
