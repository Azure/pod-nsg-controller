# Conductor Onboarding & Workflow Guide

## What is Conductor?

[Microsoft Conductor](https://github.com/microsoft/conductor) is a multi-agent
workflow orchestrator that coordinates AI agents through YAML-defined pipelines.
We use it to implement each phase of `docs/SPECIFICATION.md` through a
test-driven development (TDD) workflow.

## Installation

```bash
# Option A: One-line install (recommended)
curl -sSfL https://aka.ms/conductor/install.sh | sh

# Option B: Install via uv (Python package manager)
uv tool install conductor-ai

# Verify installation
conductor --version
```

### Prerequisites

- **GitHub Copilot CLI** (provider for AI agents):
  ```bash
  gh extension install github/gh-copilot
  gh auth login  # if not already authenticated
  ```
- **Go 1.24+** (for building/testing the project)
- **controller-gen** (for CRD generation): `go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest`

## Workflow Architecture

The TDD phase workflow (`.conductor/phase-workflow.yaml`) orchestrates 7 agents:

```
┌──────────┐     ┌─────────────────┐     ┌───────────────────┐
│ Designer │────►│ Design Reviewer │────►│ Unit Test Writer  │
│ (Opus)   │◄────│ (Opus)          │     │ (Opus)            │
└──────────┘     └─────────────────┘     └────────┬──────────┘
  revise loop                                      │
                                                   ▼
                    ┌─────────────────────┐  ┌──────────┐
                    │ Unit Test Validator │◄─│  Coder   │
                    │ (Sonnet)           │──►│  (Opus)  │
                    └────────┬───────────┘  └──────────┘
                             │ all pass          ▲
                             ▼              fix loop
                    ┌────────────────────┐
                    │ Integration Tester │
                    │ (Opus)             │
                    └────────┬───────────┘
                             │
                             ▼
                    ┌────────────────────┐
                    │  QA Validator      │──► $end (approved)
                    │  (Opus)            │──► Coder (gaps)
                    └────────────────────┘
```

### Agent Roles

| Agent | Model | Role |
|-------|-------|------|
| **Designer** | GPT-5.3 Codex | Reads spec, researches codebase, produces implementation design |
| **Design Reviewer** | GPT-5.4 | Validates design against spec (score ≥ 85 to approve) |
| **Unit Test Writer** | Claude Opus 4.6 | Writes failing tests from TDD acceptance criteria (tests MUST fail) |
| **Coder** | Claude Opus 4.6 | Implements production code to make tests pass |
| **Unit Test Validator** | GPT-5.4 | Runs `go test` and `go vet`, reports pass/fail |
| **Integration Tester** | Claude Opus 4.6 | Writes cross-module integration tests |
| **QA Validator** | GPT-5.4 | Final gate — validates all deliverables against spec (score ≥ 90) |

## Usage

### Run a Single Phase

```bash
# Phase 1: CRD Types
conductor run .conductor/phase-workflow.yaml \
  --input phase="Phase 1: CRD Types, Scaffolding & Scheme Registration" \
  --input spec_path="docs/SPECIFICATION.md"

# Phase 2: Domain Model
conductor run .conductor/phase-workflow.yaml \
  --input phase="Phase 2: Domain Model & Desired-State Computation" \
  --input spec_path="docs/SPECIFICATION.md"

# Phase 3: Desired-State Engine
conductor run .conductor/phase-workflow.yaml \
  --input phase="Phase 3: Desired-State Diff Engine" \
  --input spec_path="docs/SPECIFICATION.md"
```

### With Web Dashboard

```bash
conductor run .conductor/phase-workflow.yaml --web \
  --input phase="Phase 1: CRD Types, Scaffolding & Scheme Registration" \
  --input spec_path="docs/SPECIFICATION.md"
```

### Verbose Output

```bash
conductor -V run .conductor/phase-workflow.yaml \
  --input phase="Phase 1: CRD Types, Scaffolding & Scheme Registration" \
  --input spec_path="docs/SPECIFICATION.md"
```

## Phase Execution Order

Phases **must** be executed in order (each depends on the previous):

| Phase | Name | Description |
|-------|------|-------------|
| 1 | CRD Types & Scaffolding | `PodASGMapping` CRD, deepcopy, scheme registration |
| 2 | Domain Model | Pure-function desired-state computation |
| 3 | Diff Engine | Diff desired vs. actual prefix sets |
| 4 | Azure Executor | Interface-based Azure client factory |
| 5 | Controller Wiring | `MappingReconciler` with controller-runtime |
| 6 | Status Reporting | CRD status subresource updates |
| 7 | Error Handling | Retry, rate-limiting, circuit breakers |
| 8 | Integration & E2E | Full end-to-end test suite |

## Workflow Outputs

After each phase completes, the workflow reports:

- `design_review_score` — Design quality (0–100)
- `unit_tests_total` / `unit_tests_passed` — Test results
- `integration_tests_pass` — Integration test status
- `qa_score` — Final QA score (0–100)
- `qa_approved` — Whether the phase passed QA
- `files_created` — List of files created/modified

## Design Artifacts

Each phase produces a design document at:
```
.conductor/designs/<phase-name>.design.md
```

These are Git-ignored and serve as working documents during the workflow.

## Customization

### Changing Models

Edit `.conductor/phase-workflow.yaml` and update the `model` field on any agent.
Options include:
- `claude-opus-4.6` — Best for complex reasoning (design, implementation)
- `claude-sonnet-4.6` — Good balance of speed/quality (review, validation)
- `gpt-4.1` — Alternative option

### Adjusting Approval Thresholds

- **Design Review**: Change `score < 85` in `design_reviewer.routes`
- **QA Validation**: Change `score >= 90` in `qa_validator.prompt`

### Adding MCP Servers

Add tool servers under `workflow.runtime.mcp_servers`:
```yaml
runtime:
  mcp_servers:
    web-search:
      command: npx
      args: ["-y", "open-websearch@latest"]
      tools: ["search"]
```

## Troubleshooting

| Issue | Fix |
|-------|-----|
| `conductor: command not found` | Run the install script or add to PATH |
| Agent loops indefinitely | Check `limits.max_iterations` (default: 80) |
| Tests compile but shouldn't | Unit Test Writer should create minimal stubs only |
| Design rejected repeatedly | Lower the threshold or review spec for ambiguity |
