# MCP server

The Machinist control plane exposes its API as Model Context Protocol tools
over the **streamable HTTP** transport, in the same binary as the web UI. No
sidecar process is needed.

- **Endpoint:** `POST http://<control-plane>/mcp`
- **Auth:** the same scoped bearer tokens as the HTTP API
  (`Authorization: Bearer mmt_…`). Write tools require a write-scope token;
  with `auth = "none"` all tools are open (loopback/Tailscale only).
- **Transport:** JSON response mode. `initialize`, `ping`, `tools/list` and
  `tools/call` are supported; notifications return `202 Accepted`; unsupported
  methods return a JSON-RPC error; `GET`/`DELETE /mcp` return `405` (no SSE
  streams, no session resumption).

Point any MCP client at the endpoint:

```json
{
  "mcpServers": {
    "machinist": {
      "type": "http",
      "url": "http://127.0.0.1:7331/mcp",
      "headers": { "Authorization": "Bearer mmt_…" }
    }
  }
}
```

## Tool list

Read tools need a read-scope (or write-scope) token; write tools need write
scope.

| Tool | Scope | Arguments | Description |
| --- | --- | --- | --- |
| `list_jobs` | read | `state?` | Jobs newest first with runs; optional state filter. |
| `get_job` | read | `id` | One job with prompt, state, attempts and runs. |
| `get_run` | read | `id` | One run with exit code, timings and final message. |
| `get_transcript` | read | `run_id` | The run's event transcript. |
| `get_board` | read | — | Full board state: jobs, workers, triggers. |
| `list_review_profiles` | read | — | Stored review profiles. |
| `get_settings` | read | — | All settings key/value pairs. |
| `get_setting` | read | `key` | One setting; errors if missing. |
| `workflows_get` | read | `project` | A project's workflow config: default type, label mappings and job types with ordered stages. |
| `job_types_list` | read | `project` | A project's job types and default type. |
| `queue_job` | write | `repository` (required), `command?`, `workflow?`, `title?`, `spec?`, `source_url?`, `prompt?`, `model?`, `project?`, `job_type?` | Queue a job; returns the job id. |
| `cancel_job` | write | `job_id` | Cancel the job and its latest run, or a queued job that has no runs yet. |
| `requeue_job` | write | `job_id`, `previous_process_stopped?` | Retry a failed/blocked/interrupted/cancelled job. |
| `set_setting` | write | `key`, `value` | Write one settings key/value pair. |
| `workflows_put` | write | `project`, `config` | Replace a project's workflow config (validated). |
| `job_types_put` | write | `project`, `name`, `stages` | Add or replace one job type. |
| `job_types_delete` | write | `project`, `name` | Delete one job type. |

Tool results are JSON rendered as text content; failures return
`isError: true` with the reason. Every change made through the tools is
audit-logged with the token's name, like the HTTP API.
