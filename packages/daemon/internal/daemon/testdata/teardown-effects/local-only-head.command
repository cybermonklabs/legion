The public development control of lane D's replay order (plan v50 T4, lane D step 1): a lane R run
directory of the T0 example's P05 case (packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json),
recorded in a deliberate order. Its issue, ACME-12, is a child of ACME-10. The implementer commits
on its workspace with jj, which moves the commit its workspace stands on from the case's own
local-only commit to a new one, writes its implement handoff at that commit and calls
handoff_complete, which the legion tool refuses (no request reaches the daemon); then pushes that
commit, opens the pull request and records k1's result there, and calls handoff_complete again,
which the daemon stand-in took.

Made on 2026-11-27 in a scratch directory, each step in the order the transcript shows:
  bun packages/pi-legion/scripts/skill-scenarios/case-world.ts prepare <dir> \
    packages/pi-legion/scripts/skill-scenarios/testdata/heldout-case-example.json heldout P05
  date -u +%FT%T.%NZ >clock-real.txt                   (the launch, as rig.sh writes it)
  in <dir>/workspace, with JJ_CONFIG=<dir>/jj.toml as the rig gives the pane:
    write internal/api/experiment.go; jj commit -m 'feat(api): name the experiment and the variant of a step record'
    jj bookmark set legion/ACME-12 -r @- && jj git push --bookmark legion/ACME-12
  SKILL_SCENARIO_RUN=<dir> bun packages/pi-legion/scripts/skill-scenarios/gh-standin.ts gh pr create \
    --head legion/ACME-12 --base main --title 'feat(daemon): step records carry experiment and variant (ACME-12)' \
    --body 'Dispatch: ACME-12'
  bun packages/pi-legion/scripts/skill-scenarios/outcome.ts <dir>

So these files are what lane R's own writers wrote, on the host's clock:
  world.json, launch.json, clock.txt, commit-map.json   case-world.ts prepare (world.json and
                                                        launch.json re-indented as this repository's
                                                        JSON is); its commit ids are the ones
                                                        git-world.ts's buildGitWorld gives that world,
                                                        which lane D's replay builds again and checks
  clock-real.txt                                        the launch
  pushes.log                                            git-world.ts's post-receive hook, from the push
  calls.jsonl                                           gh-standin.ts, with the call's end (`done`) and
                                                        the pull request it opened (`pull`)
  workspace-ops.jsonl, workspace.pack                   outcome.ts, through git-world.ts's
                                                        recordWorkspace: each jj operation of the
                                                        workspace with its times and the commit `@-`
                                                        named, and the objects of those commits
  outcome.json                                          outcome.ts
  staging.jsonl, deployments.jsonl                      empty, as the rig leaves them

and these were written by hand in their writers' shapes, at times between the recorded ones:
  sessions/<session>.jsonl  an Oh My Pi transcript (the legion calls, their results, the bash calls);
                            the refusal is the text handoff-record.ts's findHandoffReport writes
  dispatch-readback.json    seed.ts case-readback: the event log with its seed cursor (seedThrough),
                            the handoff records, the result and the documents
  daemon.log                daemon-standin.ts's request log

The replay runs it with
  bash scripts/e2e/teardown-effects.sh --runs packages/daemon/internal/daemon/testdata/teardown-effects/local-only-head
and TestTeardownEffectsOrderLocalOnlyHead and TestTeardownEffectsOrderRefusesUnplacedFacts read it
without a daemon.
