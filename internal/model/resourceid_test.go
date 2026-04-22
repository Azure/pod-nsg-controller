package model

import (
	"strings"
	"testing"
)

func TestPhase2_T21_ParseValidASGResourceID(t *testing.T) {
	input := "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	parsed, err := ParseASGResourceID(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.SubscriptionID != "sub-123" {
		t.Errorf("SubscriptionID = %q, want %q", parsed.SubscriptionID, "sub-123")
	}
	if parsed.ResourceGroup != "my-rg" {
		t.Errorf("ResourceGroup = %q, want %q", parsed.ResourceGroup, "my-rg")
	}
	if parsed.ASGName != "my-asg" {
		t.Errorf("ASGName = %q, want %q", parsed.ASGName, "my-asg")
	}
	if parsed.FullResourceID == "" {
		t.Errorf("FullResourceID should not be empty")
	}
	// FullResourceID must have canonical casing for static segments
	if !strings.Contains(parsed.FullResourceID, "Microsoft.Network") {
		t.Errorf("FullResourceID = %q, want canonical casing with Microsoft.Network", parsed.FullResourceID)
	}
	if !strings.Contains(parsed.FullResourceID, "applicationSecurityGroups") {
		t.Errorf("FullResourceID = %q, want canonical casing with applicationSecurityGroups", parsed.FullResourceID)
	}
}

func TestPhase2_T22_ParseInvalidResourceIDWrongProvider(t *testing.T) {
	input := "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/my-vm"
	_, err := ParseASGResourceID(input)
	if err == nil {
		t.Fatal("expected error for wrong provider, got nil")
	}
	if !strings.Contains(err.Error(), "") {
		// Error should be descriptive
	}
	// Verify it's a descriptive error (not just empty)
	if len(err.Error()) == 0 {
		t.Errorf("error message should be descriptive, got empty string")
	}
}

func TestPhase2_T23_ParseResourceIDMixedCaseProviderPath(t *testing.T) {
	input := "/subscriptions/sub-ABC/resourceGroups/My-RG/providers/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/My-ASG"
	parsed, err := ParseASGResourceID(input)
	if err != nil {
		t.Fatalf("unexpected error for mixed-case input: %v", err)
	}
	// Dynamic segments preserve original case
	if parsed.SubscriptionID != "sub-ABC" {
		t.Errorf("SubscriptionID = %q, want %q (preserve original case)", parsed.SubscriptionID, "sub-ABC")
	}
	if parsed.ResourceGroup != "My-RG" {
		t.Errorf("ResourceGroup = %q, want %q (preserve original case)", parsed.ResourceGroup, "My-RG")
	}
	if parsed.ASGName != "My-ASG" {
		t.Errorf("ASGName = %q, want %q (preserve original case)", parsed.ASGName, "My-ASG")
	}
	// FullResourceID must have canonical casing for static segments
	if !strings.Contains(parsed.FullResourceID, "Microsoft.Network") {
		t.Errorf("FullResourceID = %q, want canonical 'Microsoft.Network'", parsed.FullResourceID)
	}
	if !strings.Contains(parsed.FullResourceID, "applicationSecurityGroups") {
		t.Errorf("FullResourceID = %q, want canonical 'applicationSecurityGroups'", parsed.FullResourceID)
	}
}

func TestPhase2_ParseASGResourceID_RejectsMissingLeadingSlash(t *testing.T) {
	input := "subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	_, err := ParseASGResourceID(input)
	if err == nil {
		t.Fatal("expected error for missing leading slash, got nil")
	}
}

func TestPhase2_ParseASGResourceID_RejectsWrongSegmentCount(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"too few segments", "/subscriptions/sub-123/resourceGroups/my-rg"},
		{"too many segments", "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg/extra"},
		{"empty string", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseASGResourceID(tc.input)
			if err == nil {
				t.Fatalf("expected error for input %q, got nil", tc.input)
			}
		})
	}
}

func TestPhase2_ParseASGResourceID_RejectsTrailingSlash(t *testing.T) {
	input := "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg/"
	_, err := ParseASGResourceID(input)
	if err == nil {
		t.Fatal("expected error for trailing slash, got nil")
	}
}

func TestPhase2_ParseASGResourceID_RejectsEmptyDynamicSegments(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty subscription", "/subscriptions//resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg"},
		{"empty resource group", "/subscriptions/sub-123/resourceGroups//providers/Microsoft.Network/applicationSecurityGroups/my-asg"},
		{"empty ASG name", "/subscriptions/sub-123/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseASGResourceID(tc.input)
			if err == nil {
				t.Fatalf("expected error for input %q, got nil", tc.input)
			}
		})
	}
}

// --- Additional coverage below ---

func TestPhase2_ParseASGResourceID_FullCanonicalReconstruction(t *testing.T) {
	// Verify FullResourceID is rebuilt with canonical static casing while preserving dynamic values.
	input := "/SUBSCRIPTIONS/Sub-XYZ/RESOURCEGROUPS/My-RG/PROVIDERS/microsoft.network/applicationsecuritygroups/My-ASG"
	parsed, err := ParseASGResourceID(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "/subscriptions/Sub-XYZ/resourceGroups/My-RG/providers/Microsoft.Network/applicationSecurityGroups/My-ASG"
	if parsed.FullResourceID != want {
		t.Errorf("FullResourceID = %q, want %q", parsed.FullResourceID, want)
	}
}

func TestPhase2_ParseASGResourceID_RejectsWrongStaticSegments(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		errSubstring string
	}{
		{
			name:         "wrong subscriptions keyword",
			input:        "/subscription/sub-1/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg",
			errSubstring: "subscriptions",
		},
		{
			name:         "wrong resourceGroups keyword",
			input:        "/subscriptions/sub-1/resourceGroup/rg/providers/Microsoft.Network/applicationSecurityGroups/asg",
			errSubstring: "resourceGroups",
		},
		{
			name:         "wrong providers keyword",
			input:        "/subscriptions/sub-1/resourceGroups/rg/provider/Microsoft.Network/applicationSecurityGroups/asg",
			errSubstring: "providers",
		},
		{
			name:         "wrong provider namespace",
			input:        "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Compute/applicationSecurityGroups/asg",
			errSubstring: "Microsoft.Network",
		},
		{
			name:         "wrong resource type",
			input:        "/subscriptions/sub-1/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/nsg",
			errSubstring: "applicationSecurityGroups",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseASGResourceID(tc.input)
			if err == nil {
				t.Fatalf("expected error for input %q, got nil", tc.input)
			}
			if !strings.Contains(err.Error(), tc.errSubstring) {
				t.Errorf("error %q should reference %q", err.Error(), tc.errSubstring)
			}
		})
	}
}

func TestPhase2_ParseASGResourceID_RejectsOnlySlash(t *testing.T) {
	_, err := ParseASGResourceID("/")
	if err == nil {
		t.Fatal("expected error for single slash, got nil")
	}
}

func TestPhase2_ParseASGResourceID_PreservesDynamicSegmentCasingExactly(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantSub string
		wantRG  string
		wantASG string
	}{
		{
			name:    "all lowercase dynamic",
			input:   "/subscriptions/abc/resourceGroups/myrg/providers/Microsoft.Network/applicationSecurityGroups/myasg",
			wantSub: "abc",
			wantRG:  "myrg",
			wantASG: "myasg",
		},
		{
			name:    "all uppercase dynamic",
			input:   "/subscriptions/ABC/resourceGroups/MYRG/providers/Microsoft.Network/applicationSecurityGroups/MYASG",
			wantSub: "ABC",
			wantRG:  "MYRG",
			wantASG: "MYASG",
		},
		{
			name:    "GUID-style subscription",
			input:   "/subscriptions/550e8400-e29b-41d4-a716-446655440000/resourceGroups/rg-1/providers/Microsoft.Network/applicationSecurityGroups/asg-1",
			wantSub: "550e8400-e29b-41d4-a716-446655440000",
			wantRG:  "rg-1",
			wantASG: "asg-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseASGResourceID(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed.SubscriptionID != tc.wantSub {
				t.Errorf("SubscriptionID = %q, want %q", parsed.SubscriptionID, tc.wantSub)
			}
			if parsed.ResourceGroup != tc.wantRG {
				t.Errorf("ResourceGroup = %q, want %q", parsed.ResourceGroup, tc.wantRG)
			}
			if parsed.ASGName != tc.wantASG {
				t.Errorf("ASGName = %q, want %q", parsed.ASGName, tc.wantASG)
			}
		})
	}
}

func TestPhase2_ParseASGResourceID_FullResourceIDRoundTrips(t *testing.T) {
	// Parsing a canonical FullResourceID again must produce identical output.
	// This is the idempotency contract for canonical reconstruction.
	input := "/subscriptions/Sub-XYZ/resourceGroups/My-RG/providers/MICROSOFT.NETWORK/APPLICATIONSECURITYGROUPS/My-ASG"
	first, err := ParseASGResourceID(input)
	if err != nil {
		t.Fatalf("first parse: unexpected error: %v", err)
	}
	second, err := ParseASGResourceID(first.FullResourceID)
	if err != nil {
		t.Fatalf("second parse (round-trip): unexpected error: %v", err)
	}
	if second.FullResourceID != first.FullResourceID {
		t.Errorf("round-trip FullResourceID: first=%q, second=%q — not idempotent", first.FullResourceID, second.FullResourceID)
	}
	if second.SubscriptionID != first.SubscriptionID {
		t.Errorf("round-trip SubscriptionID: first=%q, second=%q", first.SubscriptionID, second.SubscriptionID)
	}
	if second.ResourceGroup != first.ResourceGroup {
		t.Errorf("round-trip ResourceGroup: first=%q, second=%q", first.ResourceGroup, second.ResourceGroup)
	}
	if second.ASGName != first.ASGName {
		t.Errorf("round-trip ASGName: first=%q, second=%q", first.ASGName, second.ASGName)
	}
}

func TestPhase2_ParseASGResourceID_WhitespaceInputs(t *testing.T) {
	// Inputs where the structure is invalid due to whitespace.
	t.Run("whitespace only", func(t *testing.T) {
		_, err := ParseASGResourceID("   ")
		if err == nil {
			t.Fatal("expected error for whitespace-only input, got nil")
		}
	})
	t.Run("leading whitespace breaks leading slash", func(t *testing.T) {
		_, err := ParseASGResourceID(" /subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg")
		if err == nil {
			t.Fatal("expected error for leading whitespace, got nil")
		}
	})

	// Whitespace within dynamic segments is accepted — the parser validates structure
	// and static keywords only. Dynamic segment content is preserved as-is.
	t.Run("trailing space in ASG name is accepted", func(t *testing.T) {
		parsed, err := ParseASGResourceID("/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parsed.ASGName != "asg " {
			t.Errorf("ASGName = %q, want %q (trailing space preserved)", parsed.ASGName, "asg ")
		}
	})
	t.Run("tab in subscription is accepted", func(t *testing.T) {
		parsed, err := ParseASGResourceID("/subscriptions/sub\t1/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if parsed.SubscriptionID != "sub\t1" {
			t.Errorf("SubscriptionID = %q, want %q (tab preserved)", parsed.SubscriptionID, "sub\t1")
		}
	})
}

func TestPhase2_ParseASGResourceID_SpecialCharactersInDynamicSegments(t *testing.T) {
	// Dynamic segments with dots, underscores, numbers, etc. should parse fine.
	tests := []struct {
		name    string
		input   string
		wantSub string
		wantRG  string
		wantASG string
	}{
		{
			name:    "dots in segments",
			input:   "/subscriptions/sub.123/resourceGroups/rg.east/providers/Microsoft.Network/applicationSecurityGroups/asg.web.v2",
			wantSub: "sub.123",
			wantRG:  "rg.east",
			wantASG: "asg.web.v2",
		},
		{
			name:    "underscores in segments",
			input:   "/subscriptions/sub_123/resourceGroups/rg_east/providers/Microsoft.Network/applicationSecurityGroups/asg_web",
			wantSub: "sub_123",
			wantRG:  "rg_east",
			wantASG: "asg_web",
		},
		{
			name:    "dashes and numbers",
			input:   "/subscriptions/550e8400-e29b-41d4-a716-446655440000/resourceGroups/rg-prod-eastus2-001/providers/Microsoft.Network/applicationSecurityGroups/asg-frontend-tier1",
			wantSub: "550e8400-e29b-41d4-a716-446655440000",
			wantRG:  "rg-prod-eastus2-001",
			wantASG: "asg-frontend-tier1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseASGResourceID(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed.SubscriptionID != tc.wantSub {
				t.Errorf("SubscriptionID = %q, want %q", parsed.SubscriptionID, tc.wantSub)
			}
			if parsed.ResourceGroup != tc.wantRG {
				t.Errorf("ResourceGroup = %q, want %q", parsed.ResourceGroup, tc.wantRG)
			}
			if parsed.ASGName != tc.wantASG {
				t.Errorf("ASGName = %q, want %q", parsed.ASGName, tc.wantASG)
			}
		})
	}
}

func TestPhase2_ParseASGResourceID_IdempotentCanonicalCasing(t *testing.T) {
	// Parsing an already-canonical ID should produce FullResourceID identical to input.
	canonical := "/subscriptions/my-sub/resourceGroups/my-rg/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
	parsed, err := ParseASGResourceID(canonical)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.FullResourceID != canonical {
		t.Errorf("FullResourceID = %q, want %q — canonical input should pass through unchanged", parsed.FullResourceID, canonical)
	}
}

func TestPhase2_ParseASGResourceID_ErrorMessagesAreDescriptive(t *testing.T) {
	// Every rejection path must produce a non-empty error message.
	inputs := []string{
		"",
		"no-leading-slash",
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg/",
		"/subscriptions/sub/resourceGroups/rg",
		"/subscriptions//resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/asg",
		"/subscriptions/sub/resourceGroups//providers/Microsoft.Network/applicationSecurityGroups/asg",
		"/subscriptions/sub/resourceGroups/rg/providers/Microsoft.Network/applicationSecurityGroups/",
		"/subscriptions/sub/resourceGroups/rg/providers/Wrong.Provider/applicationSecurityGroups/asg",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			_, err := ParseASGResourceID(input)
			if err == nil {
				t.Fatalf("expected error for %q", input)
			}
			if len(err.Error()) == 0 {
				t.Errorf("error message must be descriptive (non-empty)")
			}
		})
	}
}
