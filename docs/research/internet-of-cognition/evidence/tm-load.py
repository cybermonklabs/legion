#  Load the Legion phase workflow with MAS Lab's own loaders.
#  Run from /legion/scratch/tmp/infra3134/in/out with the venv built in test-maslab.md.
from __future__ import annotations

import json
import sys
from importlib.metadata import version
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent


def head(text: str) -> None:
    print(f"\n=== {text} ===")


head("A. environment")
print("python", sys.version.split()[0])
for name in ("mas-lab-runtime", "mas-lab-ctl", "mas-lab-library-standard"):
    print(name, version(name))

head("B. the document as PyYAML reads it (YAML 1.1)")
doc = yaml.safe_load((HERE / "legion-workflow.yaml").read_text(encoding="utf-8"))
print("apiVersion", doc["apiVersion"], "kind", doc["kind"])
params = doc["spec"]["design_pattern"]["params"]
true_keys = sorted(name for name, body in params["states"].items() if True in body)
print("states whose transition key loaded as the boolean True:", len(true_keys))
print(true_keys)

head("C. their own table loader: normalize_table + validate_definition")
from mas.library.standard.plugins.design_patterns.state_machine import (  # noqa: E402
    OPERATORS,
    RUN_KINDS,
    STATE_KEYS,
    TRANSITION_KEYS,
    StateMachinePlugin,
    normalize_table,
    validate_definition,
)

table = normalize_table(params)
problems = validate_definition(table)
print("validate_definition problems:", problems)

head("D. what the table holds")
states = table["states"]
transitions = sum(len(body.get("on") or []) for body in states.values())
print("states:", len(states))
print("transitions:", transitions)
print("start:", table["start"])
end_states = [name for name, body in states.items() if body.get("run") == "end"]
print("end states:", end_states)

head("E. the vocabulary of one transition and of one state")
print("TRANSITION_KEYS:", sorted(TRANSITION_KEYS))
print("RUN_KINDS:", list(RUN_KINDS))
print("OPERATORS:", list(OPERATORS))
print("STATE_KEYS[delegate]:", sorted(STATE_KEYS["delegate"]))
print("effect key in TRANSITION_KEYS:", any("effect" in k for k in TRANSITION_KEYS))
print("guard key in TRANSITION_KEYS:", any("guard" in k for k in TRANSITION_KEYS))
print("trigger key in TRANSITION_KEYS:", any("trigger" in k for k in TRANSITION_KEYS))

head("F. the runtime spec loader: parse_agent_spec")
from mas.runtime.spec import parse_agent_spec  # noqa: E402

try:
    kernel_config, obs = parse_agent_spec(doc["spec"], manifest_dir=HERE)
    print("KernelConfig pattern_plugin_id:", kernel_config.pattern_plugin_id)
    print("KernelConfig max_auto_steps:", kernel_config.max_auto_steps)
    print("observability binding:", obs)
except Exception as exc:  # the refusal, recorded as it comes
    print(f"parse_agent_spec refused: {type(exc).__name__}: {exc}")

head("G. the plugin's own load of the table (_configure)")
plugin = StateMachinePlugin()
state = plugin._make_state()
state.participants = ["daemon", "planner", "implementer", "tester", "reviewer", "architect", "merger"]
state.original_task = "run the issue"
print("with the seven agents the table names:", plugin._configure(state, kernel_config))
print("state.current after load:", repr(state.current), "vars:", state.vars)
state2 = plugin._make_state()
state2.participants = []
state2.original_task = "run the issue"
print("with no agents:", plugin._configure(state2, kernel_config))

head("H. a run needs a model: what is there with no key")
import os  # noqa: E402

from mas.runtime.registry import get_registry  # noqa: E402

print("OPENAI_API_KEY set:", "OPENAI_API_KEY" in os.environ)
print("MAS_LLM_API_KEY set:", "MAS_LLM_API_KEY" in os.environ)
print("models the manifest names:", doc["spec"]["models"])
print("design_pattern plugins the registry holds:", sorted(get_registry().list_names("design_pattern")))

head("I. the other format: kind Workflow, validated by their validator")
from mas.ctl.validate import validate_file  # noqa: E402

result = validate_file(HERE / "legion-workflow-kind.yaml")
print("ok:", result.ok, "kind:", result.kind)
for issue in result.issues:
    print(f"  [{issue.level}] {issue.message} @ {issue.path}")
wf = yaml.safe_load((HERE / "legion-workflow-kind.yaml").read_text(encoding="utf-8"))
print("nodes:", len(wf["spec"]["nodes"]), "edges:", len(wf["spec"]["edges"]))
print("edge keys:", sorted({k for edge in wf["spec"]["edges"] for k in edge}))

head("J. the agent manifest through their validator")
result = validate_file(HERE / "legion-workflow.yaml")
print("ok:", result.ok, "kind:", result.kind)
for issue in result.issues:
    print(f"  [{issue.level}] {issue.message} @ {issue.path}")

head("K. the table as JSON, for the record")
print(json.dumps({"states": len(states), "transitions": transitions}, sort_keys=True))
