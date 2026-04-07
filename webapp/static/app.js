const state = {
  runs: [],
  selectedRunId: null,
  points: [],
  analysis: null,
  runMeta: null,
  thresholdBps: 10,
  bucketSizeBps: 1,
  detailStart: 0,
  detailEnd: 0,
  selectionAnchor: null,
};

const els = {
  runSelect: document.getElementById("run-select"),
  runMeta: document.getElementById("run-meta"),
  uploadInput: document.getElementById("upload-input"),
  uploadButton: document.getElementById("upload-button"),
  uploadStatus: document.getElementById("upload-status"),
  thresholdInput: document.getElementById("threshold-input"),
  bucketInput: document.getElementById("bucket-input"),
  refreshAnalysis: document.getElementById("refresh-analysis"),
  detailStart: document.getElementById("detail-start"),
  detailEnd: document.getElementById("detail-end"),
  selectionLabel: document.getElementById("selection-label"),
  heroTitle: document.getElementById("hero-title"),
  statusBanner: document.getElementById("status-banner"),
  overviewChart: document.getElementById("overview-chart"),
  detailChart: document.getElementById("detail-chart"),
  abSummary: document.getElementById("ab-summary"),
  baSummary: document.getElementById("ba-summary"),
  abHistogram: document.getElementById("ab-histogram"),
  baHistogram: document.getElementById("ba-histogram"),
};

function setStatus(message, tone = "neutral") {
  const tones = {
    neutral: "rgba(255, 255, 255, 0.7)",
    ok: "rgba(33, 92, 66, 0.12)",
    warn: "rgba(139, 90, 23, 0.14)",
    error: "rgba(162, 50, 35, 0.12)",
  };
  els.statusBanner.textContent = message;
  els.statusBanner.style.background = tones[tone] || tones.neutral;
}

async function fetchJson(path, options = {}) {
  const response = await fetch(path, options);
  const text = await response.text();
  let payload = null;
  try {
    payload = text ? JSON.parse(text) : null;
  } catch {
    payload = text;
  }
  if (!response.ok) {
    throw new Error((payload && payload.error) || response.statusText);
  }
  return payload;
}

function formatDuration(durationMs) {
  if (durationMs < 1000) {
    return `${durationMs}ms`;
  }
  const seconds = durationMs / 1000;
  if (seconds < 60) {
    return `${seconds.toFixed(3)}s`;
  }
  const mins = Math.floor(seconds / 60);
  const remainder = seconds - mins * 60;
  return `${mins}m ${remainder.toFixed(3)}s`;
}

function formatTs(value) {
  return new Date(value).toLocaleString();
}

function analysisPath(runId, thresholdBps, bucketSizeBps) {
  return `/api/runs/${runId}/analysis?threshold_bps=${encodeURIComponent(thresholdBps)}&bucket_size_bps=${encodeURIComponent(bucketSizeBps)}`;
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;");
}

function renderRunSelect() {
  els.runSelect.innerHTML = "";
  if (state.runs.length === 0) {
    const option = document.createElement("option");
    option.textContent = "No ready runs";
    option.value = "";
    els.runSelect.appendChild(option);
    return;
  }

  for (const run of state.runs) {
    const option = document.createElement("option");
    option.value = String(run.id);
    option.textContent = `${run.run_name} (${run.point_count} pts)`;
    if (run.id === state.selectedRunId) {
      option.selected = true;
    }
    els.runSelect.appendChild(option);
  }
}

function updateRunMeta() {
  if (!state.runMeta) {
    els.runMeta.innerHTML = "";
    els.heroTitle.textContent = "No run selected";
    return;
  }

  els.heroTitle.textContent = state.runMeta.run_name;
  els.runMeta.innerHTML = `
    <span><strong>Pair:</strong> <code>${escapeHtml(state.runMeta.maker_exchange || "?")} / ${escapeHtml(state.runMeta.taker_exchange || "?")}</code></span>
    <span><strong>Symbol:</strong> <code>${escapeHtml(state.runMeta.symbol || "?")}</code></span>
    <span><strong>Points:</strong> <code>${escapeHtml(state.runMeta.point_count)}</code></span>
    <span><strong>Source:</strong> <code>${escapeHtml(state.runMeta.source_type)}</code></span>
  `;
}

function clampDetailWindow() {
  if (state.points.length === 0) {
    state.detailStart = 0;
    state.detailEnd = 0;
    return;
  }
  const max = state.points.length - 1;
  state.detailStart = Math.max(0, Math.min(state.detailStart, max));
  state.detailEnd = Math.max(0, Math.min(state.detailEnd, max));
  if (state.detailStart > state.detailEnd) {
    [state.detailStart, state.detailEnd] = [state.detailEnd, state.detailStart];
  }
  if (state.detailStart === state.detailEnd && max > 0) {
    state.detailEnd = Math.min(max, state.detailStart + 1);
  }
  els.detailStart.max = String(max);
  els.detailEnd.max = String(max);
  els.detailStart.value = String(state.detailStart);
  els.detailEnd.value = String(state.detailEnd);
}

function xForIndex(index, total, width, left, right) {
  if (total <= 1) {
    return left;
  }
  const span = width - left - right;
  return left + (index / (total - 1)) * span;
}

function normalizedTimestamp(point) {
  return Date.parse(point.ts_local) || 0;
}

function downsamplePoints(points, maxPoints) {
  if (points.length <= maxPoints) {
    return points;
  }
  const bucketSize = Math.max(1, Math.ceil(points.length / maxPoints));
  const sampled = [];

  for (let start = 0; start < points.length; start += bucketSize) {
    const end = Math.min(points.length, start + bucketSize);
    const first = points[start];
    let minAb = first;
    let maxAb = first;
    let minBa = first;
    let maxBa = first;

    for (let index = start + 1; index < end; index += 1) {
      const point = points[index];
      if (point.spread_ab_bps < minAb.spread_ab_bps) {
        minAb = point;
      }
      if (point.spread_ab_bps > maxAb.spread_ab_bps) {
        maxAb = point;
      }
      if (point.spread_ba_bps < minBa.spread_ba_bps) {
        minBa = point;
      }
      if (point.spread_ba_bps > maxBa.spread_ba_bps) {
        maxBa = point;
      }
    }

    sampled.push(first);
    for (const candidate of [minAb, maxAb, minBa, maxBa, points[end - 1]]) {
      if (!sampled.includes(candidate)) {
        sampled.push(candidate);
      }
    }
  }

  sampled.sort((left, right) => normalizedTimestamp(left) - normalizedTimestamp(right));
  return sampled;
}

function yForValue(value, min, max, height, top, bottom) {
  const span = height - top - bottom;
  if (span <= 0 || min === max) {
    return top + span / 2;
  }
  const ratio = (value - min) / (max - min);
  return height - bottom - ratio * span;
}

function buildPath(points, key, bounds) {
  if (points.length === 0) {
    return "";
  }
  return points
    .map((point, index) => {
      const x = xForIndex(index, points.length, bounds.width, bounds.left, bounds.right);
      const y = yForValue(point[key], bounds.min, bounds.max, bounds.height, bounds.top, bounds.bottom);
      return `${index === 0 ? "M" : "L"} ${x.toFixed(2)} ${y.toFixed(2)}`;
    })
    .join(" ");
}

function chartBounds(points) {
  let min = 0;
  let max = 0;

  for (const point of points) {
    min = Math.min(min, point.spread_ab_bps, point.spread_ba_bps);
    max = Math.max(max, point.spread_ab_bps, point.spread_ba_bps);
  }

  const padding = Math.max(5, (max - min) * 0.08 || 5);
  return { min: min - padding, max: max + padding };
}

function renderEmpty(host, message) {
  host.innerHTML = `<div class="empty-state">${escapeHtml(message)}</div>`;
}

function renderChart(host, points, detail = false) {
  if (points.length === 0) {
    renderEmpty(host, detail ? "Select a smaller window or load a run." : "No chart data available.");
    return;
  }

  const renderPoints = downsamplePoints(points, detail ? 1800 : 2400);

  const width = 900;
  const height = 280;
  const padding = { top: 20, right: 18, bottom: 26, left: 54 };
  const spreadBounds = chartBounds(renderPoints);
  const bounds = {
    ...padding,
    width,
    height,
    min: spreadBounds.min,
    max: spreadBounds.max,
  };

  const abPath = buildPath(renderPoints, "spread_ab_bps", bounds);
  const baPath = buildPath(renderPoints, "spread_ba_bps", bounds);
  const zeroY = yForValue(0, bounds.min, bounds.max, height, padding.top, padding.bottom);
  const ticks = [bounds.min, (bounds.min + bounds.max) / 2, bounds.max];
  let selectionSvg = "";

  if (!detail && state.points.length > 1) {
    const startX = xForIndex(state.detailStart, state.points.length, width, padding.left, padding.right);
    const endX = xForIndex(state.detailEnd, state.points.length, width, padding.left, padding.right);
    const x = Math.min(startX, endX);
    const w = Math.max(8, Math.abs(endX - startX));
    selectionSvg = `
      <rect class="selection-rect" x="${x}" y="${padding.top}" width="${w}" height="${height - padding.top - padding.bottom}"></rect>
      <rect class="selection-overlay" x="${padding.left}" y="${padding.top}" width="${width - padding.left - padding.right}" height="${height - padding.top - padding.bottom}"></rect>
    `;
  }

  host.innerHTML = `
    <svg class="chart-svg" viewBox="0 0 ${width} ${height}" preserveAspectRatio="none">
      <line class="grid-line" x1="${padding.left}" y1="${zeroY}" x2="${width - padding.right}" y2="${zeroY}"></line>
      ${ticks
        .map((tick) => {
          const y = yForValue(tick, bounds.min, bounds.max, height, padding.top, padding.bottom);
          return `
            <line class="grid-line" x1="${padding.left}" y1="${y}" x2="${width - padding.right}" y2="${y}"></line>
            <text class="axis-label" x="6" y="${y + 4}">${tick.toFixed(1)} bps</text>
          `;
        })
        .join("")}
      <path class="ab-line" d="${abPath}"></path>
      <path class="ba-line" d="${baPath}"></path>
      ${selectionSvg}
      <text class="axis-label" x="${padding.left}" y="${height - 6}">${escapeHtml(formatTs(renderPoints[0].ts_local))}</text>
      <text class="axis-label" x="${width - padding.right - 150}" y="${height - 6}">${escapeHtml(formatTs(renderPoints[renderPoints.length - 1].ts_local))}</text>
    </svg>
  `;

  if (!detail) {
    const overlay = host.querySelector(".selection-overlay");
    if (overlay) {
      overlay.addEventListener("pointerdown", (event) => {
        const rect = overlay.getBoundingClientRect();
        state.selectionAnchor = ((event.clientX - rect.left) / rect.width) * (state.points.length - 1);
      });
      overlay.addEventListener("pointerup", (event) => {
        if (state.selectionAnchor === null) {
          return;
        }
        const rect = overlay.getBoundingClientRect();
        const current = ((event.clientX - rect.left) / rect.width) * (state.points.length - 1);
        const start = Math.max(0, Math.min(state.points.length - 1, Math.round(Math.min(state.selectionAnchor, current))));
        const end = Math.max(0, Math.min(state.points.length - 1, Math.round(Math.max(state.selectionAnchor, current))));
        state.selectionAnchor = null;
        if (start === end) {
          return;
        }
        applyDetailWindow(start, end);
      });
    }
  }
}

function renderHistogram(host, label, buckets) {
  if (!buckets || buckets.length === 0) {
    renderEmpty(host, `No ${label} distribution available.`);
    return;
  }

  const width = 900;
  const height = 280;
  const padding = { top: 20, right: 16, bottom: 40, left: 50 };
  const innerWidth = width - padding.left - padding.right;
  const innerHeight = height - padding.top - padding.bottom;
  const maxCount = Math.max(...buckets.map((bucket) => bucket.count), 1);
  const barWidth = innerWidth / buckets.length;

  host.innerHTML = `
    <svg class="histogram-svg" viewBox="0 0 ${width} ${height}" preserveAspectRatio="none">
      ${buckets
        .map((bucket, index) => {
          const x = padding.left + index * barWidth + 2;
          const barHeight = (bucket.count / maxCount) * innerHeight;
          const y = padding.top + innerHeight - barHeight;
          const color = label === "AB" ? "var(--ab)" : "var(--ba)";
          return `
            <rect x="${x}" y="${y}" width="${Math.max(8, barWidth - 4)}" height="${barHeight}" fill="${color}" opacity="0.82"></rect>
            <text class="axis-label" x="${x}" y="${height - 14}" transform="rotate(30 ${x} ${height - 14})">${bucket.lower_bps.toFixed(0)}</text>
          `;
        })
        .join("")}
      <text class="axis-label" x="${padding.left}" y="${height - 4}">Bucket lower bound (bps)</text>
      <text class="axis-label" x="6" y="${padding.top + 10}">Count</text>
    </svg>
  `;
}

function renderSummary(host, directionLabel, direction) {
  if (!direction) {
    renderEmpty(host, `No ${directionLabel} analysis loaded.`);
    return;
  }

  const intervals = direction.intervals
    .map(
      (interval) => `
        <div class="interval-item">
          <strong>${formatDuration(interval.duration_ms)}</strong>
          <div>${escapeHtml(formatTs(interval.start_ts_local))} → ${escapeHtml(formatTs(interval.end_ts_local))}</div>
          <div class="muted">max spread ${interval.max_spread_bps.toFixed(2)} bps</div>
        </div>
      `,
    )
    .join("");

  host.innerHTML = `
    <div class="summary-grid">
      <div class="stat-card"><span>Samples</span><strong>${direction.samples}</strong></div>
      <div class="stat-card"><span>Above Threshold</span><strong>${direction.above_threshold_samples}</strong></div>
      <div class="stat-card"><span>Intervals</span><strong>${direction.opportunity_count}</strong></div>
      <div class="stat-card"><span>Total Duration</span><strong>${formatDuration(direction.total_opportunity_duration_ms)}</strong></div>
      <div class="stat-card"><span>Longest Duration</span><strong>${formatDuration(direction.longest_opportunity_duration_ms)}</strong></div>
      <div class="stat-card"><span>Best Spread</span><strong>${Number.isFinite(direction.best_opportunity_spread_bps) ? direction.best_opportunity_spread_bps.toFixed(2) : "n/a"} bps</strong></div>
    </div>
    <div class="interval-list">
      ${intervals || `<div class="empty-state">No ${directionLabel} interval crossed the current threshold.</div>`}
    </div>
  `;
}

function renderSelectionLabel() {
  if (state.points.length === 0) {
    els.selectionLabel.textContent = "No run selected.";
    return;
  }
  const start = state.points[state.detailStart];
  const end = state.points[state.detailEnd];
  els.selectionLabel.textContent = `${formatTs(start.ts_local)} → ${formatTs(end.ts_local)}`;
}

function applyDetailWindow(start, end) {
  state.detailStart = start;
  state.detailEnd = end;
  clampDetailWindow();
  renderSelectionLabel();
  renderCharts();
}

function renderCharts() {
  renderChart(els.overviewChart, state.points, false);
  const detailPoints = state.points.slice(state.detailStart, state.detailEnd + 1);
  renderChart(els.detailChart, detailPoints, true);
}

function renderAnalysis() {
  renderSummary(els.abSummary, "AB", state.analysis?.ab);
  renderSummary(els.baSummary, "BA", state.analysis?.ba);
  renderHistogram(els.abHistogram, "AB", state.analysis?.ab?.histogram);
  renderHistogram(els.baHistogram, "BA", state.analysis?.ba?.histogram);
}

async function loadAnalysis() {
  if (!state.selectedRunId) {
    return;
  }
  setStatus("Recomputing threshold-duration and histogram analytics...");
  state.thresholdBps = Number(els.thresholdInput.value);
  state.bucketSizeBps = Number(els.bucketInput.value);
  state.analysis = await fetchJson(analysisPath(state.selectedRunId, state.thresholdBps, state.bucketSizeBps));
  renderAnalysis();
  setStatus("Analysis refreshed from persisted spread points.", "ok");
}

async function loadRun(runId) {
  state.selectedRunId = Number(runId);
  if (!state.selectedRunId) {
    return;
  }
  setStatus("Loading run data...");
  const [runMeta, points, analysis] = await Promise.all([
    fetchJson(`/api/runs/${state.selectedRunId}`),
    fetchJson(`/api/runs/${state.selectedRunId}/points`),
    fetchJson(analysisPath(state.selectedRunId, els.thresholdInput.value, els.bucketInput.value)),
  ]);

  state.runMeta = runMeta;
  state.points = points;
  state.analysis = analysis;
  applyDetailWindow(0, Math.max(0, points.length - 1));
  updateRunMeta();
  renderAnalysis();
  setStatus(`Loaded ${runMeta.run_name}. Drag the overview chart to refocus detail.`, "ok");
}

async function loadRuns() {
  setStatus("Loading runs...");
  const runs = await fetchJson("/api/runs");
  state.runs = runs;
  if (runs.length === 0) {
    state.selectedRunId = null;
    state.runMeta = null;
    state.points = [];
    state.analysis = null;
    renderRunSelect();
    updateRunMeta();
    renderChart(els.overviewChart, [], false);
    renderChart(els.detailChart, [], true);
    renderAnalysis();
    setStatus("No imported runs are ready yet. Upload a raw.jsonl file to begin.", "warn");
    return;
  }

  if (!state.selectedRunId || !runs.some((run) => run.id === state.selectedRunId)) {
    state.selectedRunId = runs[0].id;
  }
  renderRunSelect();
  await loadRun(state.selectedRunId);
}

async function handleUpload() {
  const file = els.uploadInput.files?.[0];
  if (!file) {
    els.uploadStatus.textContent = "Choose a raw.jsonl file first.";
    return;
  }
  els.uploadStatus.textContent = "Uploading and importing...";
  try {
    await fetchJson("/api/uploads", {
      method: "POST",
      headers: { "X-Filename": file.name },
      body: await file.arrayBuffer(),
    });
    els.uploadStatus.textContent = `Imported ${file.name}.`;
    await loadRuns();
  } catch (error) {
    els.uploadStatus.textContent = error.message;
    setStatus(`Upload failed: ${error.message}`, "error");
  }
}

function bindEvents() {
  els.runSelect.addEventListener("change", async (event) => {
    try {
      await loadRun(event.target.value);
    } catch (error) {
      setStatus(`Failed to load run: ${error.message}`, "error");
    }
  });

  els.refreshAnalysis.addEventListener("click", async () => {
    try {
      await loadAnalysis();
    } catch (error) {
      setStatus(`Failed to refresh analysis: ${error.message}`, "error");
    }
  });

  els.detailStart.addEventListener("input", () => {
    applyDetailWindow(Number(els.detailStart.value), state.detailEnd);
  });

  els.detailEnd.addEventListener("input", () => {
    applyDetailWindow(state.detailStart, Number(els.detailEnd.value));
  });

  els.uploadButton.addEventListener("click", handleUpload);
}

async function init() {
  bindEvents();
  try {
    await loadRuns();
  } catch (error) {
    setStatus(`Failed to initialize workbench: ${error.message}`, "error");
    renderEmpty(els.overviewChart, "Workbench failed to load.");
    renderEmpty(els.detailChart, "Workbench failed to load.");
    renderAnalysis();
  }
}

init();
