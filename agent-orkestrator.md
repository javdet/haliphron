# Agent orchestrator

- An orchestrator that runs agents in a k8s cluster
- Can accept tasks through a regular API
- Can accept them over MCP

## Input contracts:
- run_agent — run a single agent. It starts, performs the task, returns the result, and is destroyed
`spec: {agent, model, role, prompt, repo, base_branch, target_branch, async}`

agent: claude-code, codex, opencode. default claude-code
prompt: the initiating request
model: sonnet, gpt, gemini flash, default opus
role: any word: coder, sre, devsecops, analytic, default empty (configured in the web UI or MCP)
repo: repository address, default empty
base_branch: which branch to make changes from, if the task is about changes, default main
target_branch: what to call the target branch? If not specified, the agent comes up with a name itself
async: whether to wait for a response or not

- run_workflow — run a series of agents according to a workflow description
`spec: {workflow, prompt, repo, base_branch, target_branch}`

workflow: full-development-process, default empty, must be specified. (available options are set in the web UI or MCP settings)
repo: repository address, default empty
prompt: the initiating request
base_branch: which branch to make changes from, if the task is about changes, default main
target_branch: what to call the target branch? If not specified, the agent comes up with a name itself


workflow: a chain of agents to run (runs may be parallel); each run is essentially a run_agent call
Workflow example
prompt -> system-designer -> frontend developer -> code-reviewer -> deployer
                          -> backend developer

I am thinking of offloading the workflow branching implementation onto claude-code itself. The system-designer's instructions say: use the resulting plan as the prompt for launching agents via run_agent (with the frontend-developer and backend-developer roles). That is, system-designer does its job, launches 2 separate agents and dies. The only question is how to get back to the summarizer agent, how to wait for the parallel tasks to finish.

- run_job — run an agent at a specific time
`spec: {agent, model, role, prompt, repo, base_branch, target_branch, schedule}`

agent: claude-code, codex, opencode. default claude-code
prompt: the initiating request
model: sonnet, gpt, gemini flash, default opus
role: any word: coder, sre, devsecops, analytic, default empty
repo: repository address, default empty
base_branch: which branch to make changes from, if the task is about changes, default main
target_branch: what to call the target branch? If not specified, the agent comes up with a name itself
schedule: "1 23 * * *" as in cron

## System components
- Frontend UI — lets you configure the solution, connect clusters, create roles and workflows. Track execution and view statistics
- K8s controller — handles the RunAgent, RunAgentWorkflow, RunAgentJob CRDs. These are specifications for launching a job in k8s. The controller launches and deletes them, and collects metrics and analytics
- mcp server — handles requests from other agents over MCP and forwards them to the backend; requires a token for authentication
- backend + API — the software core + API. All requests converge here; it creates CRDs in the cluster. Manages configuration and interacts with storage.
- config storage — configuration storage
- shared-storage — storage for work results, used both for delivering results to end consumers and for intermediate results for agents

## Inbound interfaces
1. MCP tools run_agent, run_workflow, run_job, get_agent_result, get_workflow_result
   plus tools for configuring the system without the UI
2. REST API POST /run-agent, /run_workflow, /run_job and so on
3. Chat bot — a slack bot at minimum
4. Through the UI

## Architectural decisions
- backend + API — go + gin
- frontend — react or something similar
- mcp server — go
- K8s controller — go
- config storage — help needed choosing this one
- shared-storage — and this one too

## Description of how it works
A request to run an agent arrives
run_agent
agent: claude-code
prompt: Add a new aws rds postgres database for myproject
model: opus
role: coder
repo: github.com/myorg/myproject.git
base_branch: main
target_branch: database
async: true

The backend validates what arrived and creates a CRD in the cluster with these parameters
The controller picks up this CRD and creates a job
The controller sends the task's execution state to the backend, and this is visible through the UI
Pod startup: a special image based on claude-code starts
The repository is downloaded if there is one, then it tries to install plugins from the project directory .claude/settings.<role>.json
That is, a different set of skills, agents and MCPs is installed depending on the role. Then, if there is no such file, it tries to install from ~/.claude.settings.<role>.json — these files are mounted as configmaps or secrets, and the files are defined in the UI as a fallback if they are not in the repo.
If no role is specified or the role's files are not found, it tries to install whatever is in .claude/settings.json
If there is nothing, nothing extra is installed
claude-code is started in the project with the required model and prompt
It does its work, a branch is created and a PR is opened.
The result is returned to the backend via callback. The backend returns the result to the outside
If the pod crashes it must come back up automatically; all of this is checked by the controller


## Agent images
Consider whether we need our own claude-code, codex, opencode images
We may need an init container that copies the repository. Then inside the agent container itself a custom entrypoint is started, mounted by the controller. That is where plugin installation and the claude launch command happen. The prompt is in an environment variable
The only question is how, after the agent finishes its work, to create a new branch, open a PR and send the output to the callback


## Deploy
A chart needs to be prepared for deploying all components to k8s
