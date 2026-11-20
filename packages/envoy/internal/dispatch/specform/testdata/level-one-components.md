### Legion

It runs agents to turn approved work into software changes.

#### Work coordination (component `legion-coordination`)

It admits issues, shares the capacity of a project between issues, and keeps ownership and dependencies consistent.

#### Workflows (component `legion-workflows`)

It decides which steps of a work unit can run, from the unit's workflow version, what the unit waits on and the recorded outcomes.

#### Agent runs (component `legion-runners`)

It starts, resumes and observes agents with their assigned roles.

#### Work environments (component `legion-environments`)

It provides isolated computing and repository workspaces for agent work.

#### Change integration (component `legion-change-integration`)

It publishes the change of a work unit for review and brings it into the repository under the rules of that repository.

#### Deployment settings (component `legion-settings`)

It applies the runtimes, models, role instructions and limits that a deployment chooses, and keeps the experiment group of each issue or issue tree.

#### Project controller (component `legion-controller`)

It is the one agent of a project that ranks waiting work and settles what the owners of work cannot agree on.

#### Work participation (component `legion-work-participation`)

It lets an agent in its session take its assignment, report its state and its completion, and get a reminder of open work.

#### Worker contract (interface `worker-contract`)

An agent in its session and Legion agree on compatible messages and on the work that a report is about.

### Dispatch

It holds work, documents and decisions for people and their agents.

#### Records (component `dispatch-records`)

It holds each record of Dispatch and the relations between records, and it resolves each reference to a record.

#### Document editing (component `dispatch-documents`)

It lets people and agents write and read versioned text and attachments.

#### Conversations (component `dispatch-conversations`)

It holds the threads of comments, messages and replies between people and agents, with their authors and their history.

#### Decisions and approvals (component `dispatch-decisions`)

It records questions, answers, decisions and what the person approved.

#### Inbox (component `dispatch-inbox`)

It shows a person what waits on that person, and keeps what that person read, answered or dismissed.

#### Search and actions (component `dispatch-search`)

It finds relevant records and answers or acts on a request of a person.

#### Verification (component `dispatch-verification`)

It connects statements to checks and records what each check run found.

#### Measurements (component `dispatch-measurements`)

It computes and shows work duration, delivery health, and each other measure that a project registers.

#### Caller access (component `dispatch-access`)

It decides what each person, agent or service can read or change.

#### Review of agent writing (component `dispatch-write-review`)

It reviews what an agent writes to a person before Dispatch takes it, and returns advice or a refusal.

#### Agent views (component `dispatch-agent-views`)

It lists agent sessions and shows one session while it runs, where a person can steer it.

#### Web app (component `dispatch-web-app`)

It gives each part of Dispatch one way to navigate and to lay out a page, by keyboard and on a phone.

#### Command line (component `dispatch-command-line`)

It gives each action of Dispatch as a command in a terminal, for an agent or a person.

#### Dispatch requests (interface `dispatch-requests`)

The web app, the command line and other callers use one described set of requests.

#### Approved content (interface `approved-content`)

It ties an approval to the exact text that the person approved, and shows when that text changed after.

### Envoy

It connects agent sessions to messages, outside events and controlled credentials.

#### Message delivery (component `envoy-delivery`)

It stores and delivers messages until the receiving session records them.

#### Session and role directory (component `envoy-directory`)

It records session presence, roles and the session that holds each role.

#### Outside events (component `envoy-events`)

It turns events from outside services into messages that the systems can act on.

#### Secrets broker (component `envoy-secrets`)

It authorizes credential use and forwards requests that need protected secrets.

#### Session messaging (component `envoy-session-messaging`)

It lets an agent session register its presence, and receive and send messages through Envoy.

#### Session protocol (interface `session-protocol`)

It keeps presence, role ownership and delivery consistent when a session loses its connection for a short time.

#### Build and release (component `build-and-release`)

It tests each change before or after it merges, and releases and deploys what merged.

#### Model execution (component `shared-model-execution`)

It runs a model with tools in one way for each part of the three systems that needs a model.

#### Legion to Dispatch (interface `legion-dispatch`)

It exchanges work, approvals, execution state and results between the two systems.

#### Legion to Envoy (interface `legion-envoy`)

It carries workflow notices and session presence between the two systems.

#### Dispatch to Envoy (interface `dispatch-envoy`)

Dispatch publishes each change of a record and chooses who gets it, and Envoy delivers it to sessions.

#### Outside event intake (interface `outside-event-intake`)

It brings outside changes into work state and delivery measurements.

#### Approval to work (interface `approval-gates-work`)

It tells Legion which approval of which document an issue waits on, and whether that approval stands.

#### Credential requests (interface `credential-decisions`)

Dispatch lists a credential request and links to the page of the broker, where the person decides.

#### Caller identity (interface `caller-identity`)

It carries authenticated caller identity and permissions across service boundaries.

#### Check results (interface `check-results`)

It makes each result of a check available in Dispatch with its source and its commit, from an agent or from CI.

## Components

| Id | Kind | Name | Purpose | Parent or joins |
| --- | --- | --- | --- | --- |
| legion | component | Legion | It runs agents to turn approved work into software changes. | none |
| legion-coordination | component | Work coordination | It admits issues, shares the capacity of a project between issues, and keeps ownership and dependencies consistent. | legion |
| legion-workflows | component | Workflows | It decides which steps of a work unit can run, from the unit's workflow version, what the unit waits on and the recorded outcomes. | legion |
| legion-runners | component | Agent runs | It starts, resumes and observes agents with their assigned roles. | legion |
| legion-environments | component | Work environments | It provides isolated computing and repository workspaces for agent work. | legion |
| legion-change-integration | component | Change integration | It publishes the change of a work unit for review and brings it into the repository under the rules of that repository. | legion |
| legion-settings | component | Deployment settings | It applies the runtimes, models, role instructions and limits that a deployment chooses, and keeps the experiment group of each issue or issue tree. | legion |
| legion-controller | component | Project controller | It is the one agent of a project that ranks waiting work and settles what the owners of work cannot agree on. | legion |
| legion-work-participation | component | Work participation | It lets an agent in its session take its assignment, report its state and its completion, and get a reminder of open work. | legion |
| dispatch | component | Dispatch | It holds work, documents and decisions for people and their agents. | none |
| dispatch-records | component | Records | It holds each record of Dispatch and the relations between records, and it resolves each reference to a record. | dispatch |
| dispatch-documents | component | Document editing | It lets people and agents write and read versioned text and attachments. | dispatch |
| dispatch-conversations | component | Conversations | It holds the threads of comments, messages and replies between people and agents, with their authors and their history. | dispatch |
| dispatch-decisions | component | Decisions and approvals | It records questions, answers, decisions and what the person approved. | dispatch |
| dispatch-inbox | component | Inbox | It shows a person what waits on that person, and keeps what that person read, answered or dismissed. | dispatch |
| dispatch-search | component | Search and actions | It finds relevant records and answers or acts on a request of a person. | dispatch |
| dispatch-verification | component | Verification | It connects statements to checks and records what each check run found. | dispatch |
| dispatch-measurements | component | Measurements | It computes and shows work duration, delivery health, and each other measure that a project registers. | dispatch |
| dispatch-access | component | Caller access | It decides what each person, agent or service can read or change. | dispatch |
| dispatch-write-review | component | Review of agent writing | It reviews what an agent writes to a person before Dispatch takes it, and returns advice or a refusal. | dispatch |
| dispatch-agent-views | component | Agent views | It lists agent sessions and shows one session while it runs, where a person can steer it. | dispatch |
| dispatch-web-app | component | Web app | It gives each part of Dispatch one way to navigate and to lay out a page, by keyboard and on a phone. | dispatch |
| dispatch-command-line | component | Command line | It gives each action of Dispatch as a command in a terminal, for an agent or a person. | dispatch |
| envoy | component | Envoy | It connects agent sessions to messages, outside events and controlled credentials. | none |
| envoy-delivery | component | Message delivery | It stores and delivers messages until the receiving session records them. | envoy |
| envoy-directory | component | Session and role directory | It records session presence, roles and the session that holds each role. | envoy |
| envoy-events | component | Outside events | It turns events from outside services into messages that the systems can act on. | envoy |
| envoy-secrets | component | Secrets broker | It authorizes credential use and forwards requests that need protected secrets. | envoy |
| envoy-session-messaging | component | Session messaging | It lets an agent session register its presence, and receive and send messages through Envoy. | envoy |
| build-and-release | component | Build and release | It tests each change before or after it merges, and releases and deploys what merged. | none |
| shared-model-execution | component | Model execution | It runs a model with tools in one way for each part of the three systems that needs a model. | none |
| legion-dispatch | interface | Legion to Dispatch | It exchanges work, approvals, execution state and results between the two systems. | legion, dispatch |
| legion-envoy | interface | Legion to Envoy | It carries workflow notices and session presence between the two systems. | legion, envoy |
| dispatch-envoy | interface | Dispatch to Envoy | Dispatch publishes each change of a record and chooses who gets it, and Envoy delivers it to sessions. | dispatch-conversations, dispatch-inbox, dispatch-agent-views, envoy-delivery, envoy-directory |
| dispatch-requests | interface | Dispatch requests | The web app, the command line and other callers use one described set of requests. | dispatch-web-app, dispatch-command-line, dispatch-access |
| worker-contract | interface | Worker contract | An agent in its session and Legion agree on compatible messages and on the work that a report is about. | legion-runners, legion-work-participation |
| session-protocol | interface | Session protocol | It keeps presence, role ownership and delivery consistent when a session loses its connection for a short time. | envoy-session-messaging, envoy-directory, envoy-delivery |
| outside-event-intake | interface | Outside event intake | It brings outside changes into work state and delivery measurements. | envoy-events, legion-workflows, dispatch-measurements |
| approval-gates-work | interface | Approval to work | It tells Legion which approval of which document an issue waits on, and whether that approval stands. | legion-workflows, dispatch-decisions |
| approved-content | interface | Approved content | It ties an approval to the exact text that the person approved, and shows when that text changed after. | dispatch-documents, dispatch-decisions |
| credential-decisions | interface | Credential requests | Dispatch lists a credential request and links to the page of the broker, where the person decides. | dispatch-inbox, envoy-secrets |
| caller-identity | interface | Caller identity | It carries authenticated caller identity and permissions across service boundaries. | dispatch-access, envoy-directory, envoy-secrets, legion-environments |
| check-results | interface | Check results | It makes each result of a check available in Dispatch with its source and its commit, from an agent or from CI. | dispatch-verification, build-and-release, legion-runners |
