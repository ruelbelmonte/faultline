export function time(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}

export function duration(start: string, end: string | null): string {
  if (!end) return 'running'
  const s = Math.max(0, Math.round((new Date(end).getTime() - new Date(start).getTime()) / 1000))
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} m ${s % 60} s`
}

export function percent(v: number): string {
  return `${(v * 100).toFixed(1).replace(/\.0$/, '')}%`
}

export function evidence(v: unknown): string {
  if (v === null || v === undefined) return 'none'
  return typeof v === 'object' ? JSON.stringify(v) : String(v)
}

const VERDICT_MARKS: Record<string, string> = { passed: '✓', failed: '✕', inconclusive: '?', aborted: '■' }

/** A symbol to go with the verdict word, so colour is never the only signal. */
export function verdictMark(verdict: string | null): string {
  return verdict ? (VERDICT_MARKS[verdict] ?? '') : '…'
}
