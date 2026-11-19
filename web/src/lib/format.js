export function num(v) {
  if (typeof v === 'string') return v === 'NaN' ? 'NaN' : v.startsWith('-') ? '−∞' : '∞'
  if (Number.isInteger(v) && Math.abs(v) < 1e15) return v.toLocaleString('en-US')
  const a = Math.abs(v)
  if (a !== 0 && (a < 1e-3 || a >= 1e9)) return v.toExponential(2)
  if (a >= 1e4) return v.toLocaleString('en-US', { maximumFractionDigits: 1 })
  return String(Number(v.toPrecision(4)))
}

export function duration(ms) {
  const s = Math.max(0, Math.round(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor(s / 60) % 60
  if (h) return `${h}h ${m}m`
  if (m) return `${m}m ${String(s % 60).padStart(2, '0')}s`
  return `${s}s`
}

export function ago(ms) {
  const s = Math.max(0, Math.floor(ms / 1000))
  if (s < 45) return 'just now'
  if (s < 3600) return `${Math.max(1, Math.floor(s / 60))}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}

export function frac(p) {
  return p.total > 0 ? Math.min(1, Math.max(0, p.cur / p.total)) : 0
}

export function progressText(p) {
  const pct = Math.round(frac(p) * 100)
  if (p.name === 'main' && p.total === 100) return `${pct}%`
  return `${num(p.cur)}/${num(p.total)}`
}

export function etaText(p, now) {
  if (!p.eta) return ''
  const left = Date.parse(p.eta) - now
  if (left <= 0) return '' // past the estimate: say nothing rather than "almost done"
  return `~${duration(left).replace(/ 0?0s$/, '')} left`
}

// Pinned ("stats" in pulse.yaml) first, then charted (chart order), then
// the rest; hidden ("ignore") left out.
// System metrics recorded by pulse run (keep in sync with internal/format).
const SYSTEM = {
  'sys.cpu': ['CPU', '%'], 'sys.mem': ['Memory', 'GB'], 'sys.gpu': ['GPU', '%'],
  'sys.gpu_mem': ['GPU memory', 'GB'], 'sys.disk_free': ['Free disk', 'GB'],
}
// Only these keys: a job may log its own "sys.…" values.
export const isSystem = (key) => Object.hasOwn(SYSTEM, key)

// The job's own metrics, pinned and charted first (system ones aren't here).
export function orderedMetrics(run) {
  const hidden = new Set(run.hidden ?? [])
  const keys = [...(run.pinned ?? []), ...(run.charts ?? []).flatMap((c) => c.keys)]
  const first = keys.map((k) => run.metrics.find((m) => m.name === k)).filter(Boolean)
  const pinned = new Set(run.pinned ?? [])
  return [...new Set([...first, ...run.metrics])].filter((m) => !hidden.has(m.name) && (!isSystem(m.name) || pinned.has(m.name)))
}

export function summary(run) {
  const parts = orderedMetrics(run).slice(0, 3).map((m) => `${label(m.name, run)} ${withUnit(m.value, m.name, run)}`)
  if (parts.length) return parts.join(' · ')
  if (run.progress.length) {
    const p = run.progress[0]
    return p.name === 'main' ? progressText(p) : `${label(p.name, run)} ${progressText(p)}`
  }
  return ''
}

// Keep in sync with internal/format/format.go.
const WORDS = {
  lr: 'learning rate', acc: 'accuracy', gpu: 'GPU', cpu: 'CPU', mem: 'memory', pct: '%',
  gb: 'GB', mb: 'MB', kb: 'KB', tb: 'TB', ms: 'ms', eta: 'ETA', id: 'ID', io: 'I/O',
  f1: 'F1', auc: 'AUC', mse: 'MSE', mae: 'MAE', rmse: 'RMSE', bleu: 'BLEU', rouge: 'ROUGE',
  wer: 'WER', cer: 'CER', ppl: 'perplexity', tps: 'tokens/s', qps: 'queries/s', num: '#',
  avg: 'avg', std: 'std', max: 'max', min: 'min', url: 'URL', api: 'API', ram: 'RAM',
  vram: 'VRAM', hbm: 'HBM', ttft: 'TTFT', kl: 'KL', vm: 'VM', aws: 'AWS', gcp: 'GCP',
  ssh: 'SSH', s3: 'S3', ec2: 'EC2', db: 'DB', ui: 'UI',
}

// "val_loss" → "Val loss", "gpu_util" → "GPU util", "lr" → "Learning rate".
export function humanize(key) {
  if (key === 'main') return 'Progress'
  if (own(SYSTEM, key)) return SYSTEM[key][0]
  const parts = key.replace(/([a-z0-9])([A-Z])/g, '$1 $2').split(/[_.\-/ ]+/).filter(Boolean)
  const out = parts.map((p) => own(WORDS, p.toLowerCase()) ?? p.toLowerCase()).join(' ')
  if (!out) return key
  return /^[a-z]/.test(out) ? out[0].toUpperCase() + out.slice(1) : out
}

// The run's name for a key: a configured label, else the humanised key.
export function label(name, run) {
  return own(run?.labels, name) || humanize(name)
}

// A map's own entry (never Object.prototype's: a metric may be called "constructor").
function own(map, key) {
  return map && Object.hasOwn(map, key) ? map[key] : undefined
}

// A value with the key's unit, if the job set one.
export function withUnit(v, name, run) {
  const u = own(run?.units, name) || own(SYSTEM, name)?.[1]
  const s = num(v)
  if (!u) return s
  return u === '%' ? `${s}%` : `${s} ${u}`
}

// Plain explanations of a run's state, for hover tips.
export function statusTip(run) {
  switch (run.status) {
    case 'running': return run.heartbeat ? 'Running. Pulse hears from it every few seconds.' : 'Running. Updated by pushes; marked lost if they stop.'
    case 'idle': return 'Idle: the job is between updates and not counted as running.'
    case 'done': return 'Finished successfully.'
    case 'failed': return run.exit_code != null ? `Failed with exit code ${run.exit_code}.` : 'Failed.'
    case 'lost': return 'Lost: it stopped reporting. The terminal may have closed or the machine slept.'
    case 'stopped': return 'Stopped by you.'
  }
  return run.status
}

// What an action button will do.
export function actionTip(a) {
  if (a.blocked) return `Unavailable: ${a.blocked}`
  const what = a.kind === 'send' ? `Sends “${a.detail}” to the running job`
    : a.kind === 'open' ? `Opens ${a.detail}`
    : `Runs: ${a.detail}`
  const extra = [a.confirm && 'asks to confirm first', a.when && `only while ${a.when === 'running' ? 'running' : 'finished'}`, a.source === 'file' && 'from pulse.yaml']
    .filter(Boolean).join(', ')
  return extra ? `${what}\n(${extra})` : what
}

// What an answer to an alert's question will do.
export function answerTip(ask, option) {
  const then = own(ask.then, option)
  // An alert's question (ask.rule) only acts through `then`; a job's own question gets the answer.
  if (!then) return ask.rule ? 'Records the answer; nothing else happens.' : 'Sends this answer to the job.'
  if (then === 'stop') return 'Stops the run.'
  if (then.startsWith('send:')) return `Sends “${then.slice(5)}” to the job.`
  if (then.startsWith('action:')) return `Runs the “${then.slice(7)}” action.`
  return then
}

// The run's CPU, memory, GPU and disk, as [{key, label, text, tip}].
export function systemStats(run) {
  const tips = {
    'sys.cpu': 'CPU of the job and everything it started (100% = one core)',
    'sys.mem': 'Memory the job and everything it started hold',
    'sys.gpu': 'How busy the GPU is',
    'sys.gpu_mem': 'GPU memory in use',
    'sys.disk_free': 'Free space on the disk the job runs from',
  }
  const hidden = new Set(run.hidden ?? [])
  return Object.keys(SYSTEM)
    .filter((k) => !hidden.has(k))
    .map((k) => run.metrics.find((m) => m.name === k))
    .filter((m) => m && typeof m.value === 'number')
    .map((m) => ({ key: m.name, label: label(m.name, run), text: withUnit(m.value, m.name, run), tip: tips[m.name] }))
}
