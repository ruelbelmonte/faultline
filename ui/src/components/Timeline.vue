<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import uPlot from 'uplot'
import type { Timeseries } from '../api'

const props = defineProps<{ series: Timeseries; errorLimit: number | null }>()

const host = ref<HTMLDivElement | null>(null)
let plot: uPlot | null = null
let observer: ResizeObserver | null = null
const scheme = window.matchMedia('(prefers-color-scheme: dark)')

const seconds = (iso: string) => Math.floor(new Date(iso).getTime() / 1000)

function data(): uPlot.AlignedData {
  const xs: number[] = []
  const rate: (number | null)[] = []
  const p95: (number | null)[] = []
  for (const p of props.series.series) {
    xs.push(seconds(p.t))
    rate.push(p.driver.requests > 0 ? (p.driver.errors / p.driver.requests) * 100 : null)
    p95.push(p.driver.requests > 0 ? p.driver.p95_ms : null)
  }
  return [xs, rate, p95]
}

function css(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}

// Fault windows, phase boundaries, the error-rate limit and abort markers are
// drawn over the plot. Each has a text label, so none relies on colour alone.
function overlays(u: uPlot) {
  const { ctx } = u
  const { left, top, width, height } = u.bbox
  const dpr = devicePixelRatio || 1
  const x = (iso: string) => Math.min(Math.max(u.valToPos(seconds(iso), 'x', true), left), left + width)
  ctx.save()
  ctx.font = `${11 * dpr}px system-ui, sans-serif`
  ctx.textBaseline = 'top'

  for (const w of props.series.fault_windows) {
    const x1 = x(w.start)
    const x2 = x(w.end)
    ctx.fillStyle = css('--fault-fill')
    ctx.fillRect(x1, top, Math.max(x2 - x1, 1), height)
    ctx.strokeStyle = css('--fail')
    ctx.setLineDash([4 * dpr, 3 * dpr])
    ctx.lineWidth = dpr
    ctx.strokeRect(x1, top, Math.max(x2 - x1, 1), height)
    ctx.fillStyle = css('--muted')
    ctx.fillText(`fault ${w.fault_id}`, x1 + 4 * dpr, top + 4 * dpr)
  }

  props.series.phases.forEach((ph, i) => {
    const px = x(ph.start)
    if (i > 0) {
      ctx.strokeStyle = css('--muted')
      ctx.setLineDash([2 * dpr, 4 * dpr])
      ctx.lineWidth = dpr
      ctx.beginPath()
      ctx.moveTo(px, top)
      ctx.lineTo(px, top + height)
      ctx.stroke()
    }
    ctx.fillStyle = css('--muted')
    // Alternate rows so the labels of two short phases do not overlap.
    ctx.fillText(ph.name, px + 4 * dpr, top + height - (16 + (i % 2) * 14) * dpr)
  })

  if (props.errorLimit !== null) {
    const y = u.valToPos(props.errorLimit * 100, 'pct', true)
    ctx.strokeStyle = css('--rate')
    ctx.setLineDash([dpr, 3 * dpr])
    ctx.lineWidth = dpr
    ctx.beginPath()
    ctx.moveTo(left, y)
    ctx.lineTo(left + width, y)
    ctx.stroke()
    ctx.fillStyle = css('--muted')
    ctx.textAlign = 'right'
    ctx.fillText(`error rate limit ${props.errorLimit * 100}%`, left + width - 4 * dpr, y - 15 * dpr)
    ctx.textAlign = 'left'
  }

  for (const m of props.series.markers) {
    const mx = x(m.t)
    ctx.strokeStyle = css('--fg')
    ctx.setLineDash([])
    ctx.lineWidth = 2 * dpr
    ctx.beginPath()
    ctx.moveTo(mx, top)
    ctx.lineTo(mx, top + height)
    ctx.stroke()
    ctx.fillStyle = css('--fg')
    ctx.textAlign = 'right'
    ctx.fillText(`${m.type}: ${m.detail}`, mx - 4 * dpr, top + 20 * dpr)
    ctx.textAlign = 'left'
  }
  ctx.restore()
}

function build() {
  if (!host.value) return
  plot?.destroy()
  const axis = { stroke: css('--muted'), grid: { stroke: css('--line'), width: 1 }, ticks: { stroke: css('--line') } }
  plot = new uPlot(
    {
      width: host.value.clientWidth || 800,
      height: 300,
      scales: {
        x: { time: true },
        pct: { range: [0, 100] },
        ms: { range: (_u, _min, max) => [0, Math.max(max || 0, 10) * 1.1] },
      },
      series: [
        {},
        { label: 'Error rate %', scale: 'pct', stroke: css('--rate'), width: 2, value: (_u, v) => (v == null ? '-' : `${v.toFixed(1)}%`) },
        { label: 'p95 latency ms', scale: 'ms', stroke: css('--p95'), width: 2, dash: [7, 4], value: (_u, v) => (v == null ? '-' : `${v.toFixed(1)} ms`) },
      ],
      axes: [
        axis,
        { ...axis, scale: 'pct', values: (_u, vals) => vals.map((v) => `${v}%`) },
        { ...axis, scale: 'ms', side: 1, grid: { show: false }, values: (_u, vals) => vals.map((v) => `${v} ms`), size: 70 },
      ],
      hooks: { draw: [overlays] },
    },
    data(),
    host.value,
  )
}

watch(
  () => props.series,
  () => {
    if (plot) plot.setData(data())
    else build()
  },
  { deep: true },
)
watch(() => props.errorLimit, () => plot?.redraw())

onMounted(() => {
  build()
  observer = new ResizeObserver(() => {
    if (plot && host.value) plot.setSize({ width: host.value.clientWidth, height: 300 })
  })
  if (host.value) observer.observe(host.value)
  scheme.addEventListener('change', build)
})
onBeforeUnmount(() => {
  observer?.disconnect()
  scheme.removeEventListener('change', build)
  plot?.destroy()
})
</script>

<template>
  <figure class="timeline">
    <div ref="host" role="img" aria-label="Timeline of error rate and p95 latency per second. The same numbers are in the table below."></div>
    <figcaption class="legend">
      <span>solid line: error rate (left axis)</span>
      <span>dashed line: p95 latency (right axis)</span>
      <span>shaded dashed box: fault window</span>
      <span>dotted vertical line: phase boundary</span>
      <span>thick vertical line: abort</span>
    </figcaption>
  </figure>
</template>
