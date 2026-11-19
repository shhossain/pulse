# pulse-jobs

Report progress, metrics and questions from Python to [Pulse](../../README.md).
No dependencies. Safe to leave in: if Pulse isn't reachable, every call is a no-op.

```python
import pulse

pulse.init(link="https://wandb.ai/me/runs/abc")   # optional

for batch in pulse.track(loader, name="batch"):    # a progress bar, like tqdm
    loss = train_step(batch)
    pulse.log(loss=loss, lr=lr)                    # numbers become metrics/charts

@pulse.on("save_checkpoint")                       # a button in Pulse can call this
def _():
    trainer.save()

if pulse.ask("Deploy to prod?", ["approve", "reject"], timeout="30m") == "approve":
    deploy()
```

Under `pulse run -- python train.py` it reports into that run. Run directly, it finds the
Pulse app on this Mac, or uses `PULSE_URL` / `PULSE_TOKEN` (see `pulse remote token <job>`).
In distributed jobs only rank 0 reports.
