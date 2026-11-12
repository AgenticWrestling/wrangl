# The remote MCP endpoint

`https://<controller>/mcp` serves the owner's tools over MCP (Streamable HTTP, POST only). It has the same exposure as `/api/v1`: the unix socket, and TCP behind Cloudflare Access. There is no OAuth, so a custom connector uses **No sign-in** plus the two request headers `CF-Access-Client-Id` and `CF-Access-Client-Secret` of an Access service token that a **Service Auth** policy on the application allows.

## Tools

- **Work stack** (`task_get`, `task_tree`, `task_list`, `task_create`, `task_update`, `task_move`, `task_needs_add`, `task_needs_remove`, `task_complete`, `task_reopen`, `task_cancel`, `task_delete`, `task_queue`) and `run_list`.
- **Projects**: `project_list`, `project_get`, `project_create`, `project_edit`, `project_archive`, `project_restore`, `project_delete` (needs `confirm`).
- **GitHub issues**: `issue_search`, `issue_get`, `issue_comments`, `issue_comment`, `issue_create`, `issue_edit`, `issue_close` (needs the owner's approval), `issue_link_task`.

The rest are thin bridges onto the REST API (`internal/controller/apitools`): each call is dispatched in-process to the same handler the CLI and web UI use, so the rules are identical. Destructive ones need `confirm` equal to the name.

| Tool | What it does |
|---|---|
| `question_list` | List open questions: agent questions, approvals, task and PR reviews, plan reviews. all=true includes answered ones. |
| `question_get` | Show one question with its options and detail. |
| `question_answer` | Answer a question with one of its option values and/or free text. This is how you unblock a waiting run or review. |
| `run_get` | Show one run: state, task, runner, branch, timings, error. |
| `run_cancel` | Cancel a run. |
| `run_logs` | A run's log lines (NDJSON), optionally from the last duration (since, e.g. 15m) and filtered by a regexp (grep). Large output keeps the end. |
| `run_transcript` | The agent transcript of a run (latest entries; page back with before). |
| `run_queue_order` | Set the order of queued runs: pass run ids, first = next to start. |
| `log_search` | Full-text search over run logs (FTS5 syntax), optionally within a project or runner. |
| `task_dequeue` | Take a task's queued run off the queue. |
| `task_plan` | Queue a plan run that proposes how to split a task into subtasks. |
| `task_split` | Split a task into subtasks yourself: give children (nested {title, prompt, type, children}) or an indented outline. chain=true makes each need the previous. |
| `task_ask` | Ask a read-only agent a question about a task; the answer arrives in the task thread (task_thread). |
| `task_thread` | The task's discussion thread with agents, including proposals. |
| `thread_apply` | Apply proposal n of a task-thread message (a suggested change to the task tree). |
| `project_tree` | The ranked task tree of a project (all=true includes done and cancelled). |
| `project_ask` | Ask the project chat agent about the whole project; read the reply with project_thread. |
| `project_thread` | The project chat thread, including proposals. |
| `project_thread_apply` | Apply proposal n of a project-chat message. |
| `project_runners` | The runners a project may use and its per-template instance counts. |
| `repo_list` | List linked GitHub repos, optionally for one project. |
| `repo_link` | Link a GitHub repo (owner/name) to a project. The wrangl GitHub App must be installed on it. |
| `repo_unlink` | Unlink a repo from a project. |
| `issue_list` | List synced GitHub issues, optionally for one project (state open by default; closed, all). |
| `issue_sync` | Sync a project's GitHub issues now. Returns counts per repo. |
| `issue_drift` | Show tasks whose linked issue state disagrees with the task state; repair=true fixes them. |
| `issue_to_task` | Create a task from a synced GitHub issue. |
| `pr_list` | List pull requests (state open by default; merged, closed, all). |
| `pr_get` | Show one pull request with its review, risk and gate state. |
| `pr_approve` | Approve a pull request (merges it when every gate passes). |
| `pr_request_changes` | Send a pull request back to the agent with a message. |
| `pr_resolve` | Queue Smart Merge: an agent merges the base branch in and resolves conflicts. |
| `pr_close` | Close a pull request without merging. |
| `runner_list` | List runners (all=true includes offline and terminated). |
| `runner_get` | Show one runner: state, template and version, active runs, health. |
| `runner_launch` | Launch runner instances from a template (count 1..20). Costs money. |
| `runner_cordon` | Stop a runner taking new runs. |
| `runner_uncordon` | Let a cordoned runner take runs again. |
| `runner_drain` | Cordon a runner and retire it once its runs finish. |
| `runner_delete` | Remove a runner (terminates a launched instance). Pass confirm equal to the name. |
| `template_list` | List runner templates. |
| `template_get` | Show a runner template: size, AMI, mise config, setup script, agent, limits, version. |
| `template_create` | Create a runner template from its YAML spec (see docs/wrangl/templates/*.yaml). |
| `template_update` | Replace a runner template with a new YAML spec (bumps its version; idle runners upgrade). |
| `template_delete` | Delete a runner template. Pass confirm equal to the name. |
| `target_list` | List remote targets (Mac and Android emulator hosts driven over SSH/ADB). |
| `costs_get` | Current runner spend summary and any cost lookup errors. |
| `agent_def_list` | List named agent definitions (claude, codex, ...) and the templates that allow them. |
| `agent_def_get` | Show one agent definition. |
| `agent_def_create` | Create an agent definition from a JSON spec. |
| `agent_def_update` | Replace an agent definition's description and spec. |
| `agent_def_delete` | Delete an agent definition. Pass confirm equal to the name. |
| `agent_def_allow` | Allow an agent definition on a runner template. |
| `agent_def_disallow` | Stop allowing an agent definition on a runner template. |
| `agent_list` | List the controller agents (the project and task chat agents) and which is the default. |
| `secret_list` | List the names of controller secrets. Values are write-only and never returned; set them in Settings or with the CLI. |

## Deliberately not exposed

Credential and infrastructure surfaces stay in the web UI and CLI: enrolment tokens, secret values (only `secret_list` names), push subscriptions, GitHub App setup, terminals and shells on runners, and adopting or terminating raw EC2 instances. Add a route to `apitools.Routes` to expose more; `TestCatalogue` checks the schemas.
