# Tutorials

A lesson, to be followed from beginning to end. It is not the fastest route to
any particular outcome — a [how-to guide](../how-to/) is that — but it is the
way to acquire a feel for how the pieces fit together.

## The tutorial

- [Your first agent run](your-first-agent-run.md) — install the control plane,
  connect a cluster to it, and send one agent to open a pull request. About
  thirty minutes.

It needs a Kubernetes cluster, a model provider key, and a repository you do
not mind an agent opening a pull request on.

**It has not been executed end to end.** The commands and their expected
output are derived from the code and the charts rather than captured from a
run, because the repository cannot supply a cluster or a model key. The page
says so at the top.

## Why there is only one

A second tutorial would teach the multi-step workflows that phase 1 does not
have. Until the workflow engine exists, anything else offered here would be a
how-to guide wearing a tutorial's title — which helps nobody, and is the
commonest way a documentation set goes soft.
