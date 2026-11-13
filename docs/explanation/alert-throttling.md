# About alert throttling

An alert that keeps firing is the normal case, not the exception. Alertmanager
re-sends a group every `repeat_interval`, sends it again when the group gains
or loses an alert, and in a highly available setup every peer sends it. An
agent run costs money and takes minutes. Starting one per delivery would pay
for the same investigation many times and have several agents opening rival
pull requests for one outage. So an alert receiver suppresses an alert, for a
fixed window, once it has started a run.

## What "the same alert" means

An alert is identified by Alertmanager's `fingerprint`, a hash of its full
label set. Two alerts that differ only in `pod` are different alerts. That is
deliberate: a second pod failing the same way is new information, and the
next notification's run carries it.

Annotations are not part of the identity. A changed summary or a new value in
a description does not make an alert new. A payload with no well-formed
fingerprint, from an old Alertmanager or a hand-written sender, gets one
computed from its sorted labels the same way.

## Why the window is fixed

The window starts when the run is admitted and lasts the receiver's
`throttle_seconds`, whatever the run does. It is not "suppressed while a run is
live" and not "until resolved":

- A run that fails fast would otherwise reopen the gate at once, and a
  crash-looping service would start a run every `group_interval`.
- A fixed window is something an operator can predict and explain: "this
  alert started a run at 10:02, and nothing about it will start another until
  11:02".
- `repeat_interval` and the window together decide how often a long-running
  incident is re-examined, and both are the operator's settings.

Deleting a run does not lift the suppression its alerts claimed. The window is
a fact about time, not about the run. Lifting it is an explicit act: "clear
suppression".

## Why one run carries the whole notification

Alertmanager's grouping already says which alerts belong together, and the
operator configured it. A run per alert would split one incident across
several agents, and a run per receiver would mix unrelated incidents. So a
notification is one run with every alert it carries that is not suppressed.
The prompt tells the agent how many alerts of the group were left out and
which runs are handling them, so it does not rediscover them.

## How it holds with no leader

Every API replica can receive a notification, and Alertmanager's HA peers
deliver the same group to whichever replica the load balancer picks. There is
no coordinator. The claim is made where schedule fires and deferred runs make
theirs: inside the transaction that inserts the run. That transaction runs one
`INSERT … ON CONFLICT DO UPDATE … WHERE suppressed_until <= now()` over the
run's alerts. Two outcomes are possible:

- **Every alert is claimed.** The run commits with its claims.
- **Another run claimed some alert first.** The whole insert rolls back, and
  the delivery is admitted again without those alerts.

PostgreSQL makes the conflicting statement wait for the other transaction and
then re-check its condition against what that transaction committed. That is
why concurrent deliveries end with each alert in exactly one run. Rows are
always claimed in fingerprint order, so two deliveries with overlapping alerts
cannot deadlock.

The consequence an operator sees is that a burst of identical notifications
leaves one `submitted` delivery and the rest `throttled`, however many
replicas and peers took part. The run never commits without its claim, and the
claim never commits without its run.

## The prompt treats alerts as data

Label and annotation values are written by whoever can influence a metric or
an alerting rule, which is a much wider group than the people who can start
runs. The alerts therefore go into the prompt as a JSON block between markers
carrying a random boundary, after a line saying they are data from the
monitoring system and not instructions. The operator's instruction comes
first. The role, the repository and the budget come from the receiver's
template and never from the payload. The boundary keeps a value from closing
the block early. A determined injection can still try to persuade the model,
which is why the role should be scoped for investigating alerts and nothing
broader (see [the untrusted agent pod](the-untrusted-agent-pod.md)).

## What is not throttled

The window bounds runs per alert, not runs per receiver. A storm that fires
fifty different groups starts fifty runs. Alertmanager's `group_by`,
`group_interval` and routing are the controls for that today. A per-receiver
rate ceiling is planned.
