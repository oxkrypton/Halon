import { useMemo, useState } from 'react';
import { ChevronDown, ChevronRight, AlertCircle } from 'lucide-react';
import type { TraceDetail, TraceSpan } from '../../api/traces';
import { formatMS } from '../../utils/format';

const SERVICE_COLORS = [
  '#1A73E8',
  '#1E8E3E',
  '#9334E6',
  '#E37400',
  '#00897B',
  '#D81B60',
  '#3949AB',
];

export function serviceColor(service: string, services: string[]): string {
  const idx = services.indexOf(service);
  if (idx < 0) return SERVICE_COLORS[0];
  return SERVICE_COLORS[idx % SERVICE_COLORS.length];
}

interface TreeRow {
  span: TraceSpan;
  depth: number;
  hasChildren: boolean;
}

function buildOrderedRows(spans: TraceSpan[], collapsed: Set<string>): TreeRow[] {
  const byId = new Map<string, TraceSpan>();
  const children = new Map<string, TraceSpan[]>();
  const roots: TraceSpan[] = [];

  for (const sp of spans) {
    byId.set(sp.span_id, sp);
  }
  for (const sp of spans) {
    if (sp.parent_span_id && byId.has(sp.parent_span_id)) {
      const list = children.get(sp.parent_span_id) || [];
      list.push(sp);
      children.set(sp.parent_span_id, list);
    } else {
      roots.push(sp);
    }
  }
  for (const list of children.values()) {
    list.sort((a, b) => a.start_unix_ns - b.start_unix_ns);
  }
  roots.sort((a, b) => a.start_unix_ns - b.start_unix_ns);

  const out: TreeRow[] = [];
  const visit = (sp: TraceSpan, depth: number) => {
    const kids = children.get(sp.span_id) || [];
    out.push({ span: sp, depth, hasChildren: kids.length > 0 });
    if (!collapsed.has(sp.span_id)) {
      for (const child of kids) {
        visit(child, depth + 1);
      }
    }
  };
  for (const r of roots) visit(r, 0);
  return out;
}

export function WaterfallView({
  trace,
  selectedSpanId,
  onSelectSpan,
}: {
  trace: TraceDetail;
  selectedSpanId: string | null;
  onSelectSpan: (span: TraceSpan) => void;
}) {
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set());
  const rows = useMemo(() => buildOrderedRows(trace.spans, collapsed), [trace.spans, collapsed]);
  const totalMs = Math.max(trace.duration_ms, 0.01);

  const toggleCollapse = (spanId: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(spanId)) next.delete(spanId);
      else next.add(spanId);
      return next;
    });
  };

  const ticks = [0, 0.25, 0.5, 0.75, 1];

  return (
    <div className="gc-card overflow-hidden">
      {/* Service Legend */}
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[#DADCE0] bg-[#F8F9FA] px-4 py-2">
        <div className="flex flex-wrap items-center gap-3 text-[12px]">
          {trace.services.map((svc) => (
            <span key={svc} className="inline-flex items-center gap-1.5 text-[#3C4043]">
              <span
                className="h-2.5 w-2.5 rounded-sm"
                style={{ backgroundColor: serviceColor(svc, trace.services) }}
              />
              <span className="font-mono">{svc}</span>
            </span>
          ))}
        </div>
        <div className="text-[11.5px] text-[#5F6368] tabular-nums">
          共 {trace.spans.length} 个 Span · 总耗时 <strong className="text-[#202124]">{formatMS(trace.duration_ms)}</strong>
        </div>
      </div>

      {/* Header ruler */}
      <div className="grid grid-cols-[minmax(240px,36%)_1fr] border-b border-[#DADCE0] bg-white text-[11px] text-[#5F6368]">
        <div className="px-4 py-2 font-medium border-r border-[#E8EAED]">Service &amp; Operation</div>
        <div className="relative px-3 py-2">
          <div className="relative h-4">
            {ticks.map((ratio) => (
              <span
                key={ratio}
                className="absolute top-0 -translate-x-1/2 font-mono tabular-nums first:translate-x-0 last:-translate-x-full"
                style={{ left: `${ratio * 100}%` }}
              >
                {formatMS(totalMs * ratio)}
              </span>
            ))}
          </div>
        </div>
      </div>

      {/* Span rows */}
      <div className="divide-y divide-[#F1F3F4]">
        {rows.map(({ span, depth, hasChildren }) => {
          const leftPct = Math.max(0, Math.min(99.5, (span.offset_ms / totalMs) * 100));
          const widthPct = Math.max(0.6, Math.min(100 - leftPct, (span.duration_ms / totalMs) * 100));
          const isSelected = span.span_id === selectedSpanId;
          const isErr = span.status === 'error';
          const barColor = isErr ? '#D93025' : serviceColor(span.service_name, trace.services);

          return (
            <div
              key={span.span_id}
              onClick={() => onSelectSpan(span)}
              className={`grid grid-cols-[minmax(240px,36%)_1fr] cursor-pointer transition-colors ${
                isSelected ? 'bg-[#E8F0FE]/70' : 'hover:bg-[#F8F9FA]'
              }`}
            >
              <div
                className="flex items-center gap-1.5 border-r border-[#E8EAED] py-2 pr-3 min-w-0"
                style={{ paddingLeft: `${12 + depth * 16}px` }}
              >
                {hasChildren ? (
                  <button
                    type="button"
                    onClick={(e) => {
                      e.stopPropagation();
                      toggleCollapse(span.span_id);
                    }}
                    className="p-0.5 rounded hover:bg-[#E8EAED] text-[#5F6368] shrink-0"
                  >
                    {collapsed.has(span.span_id) ? (
                      <ChevronRight className="h-3.5 w-3.5" />
                    ) : (
                      <ChevronDown className="h-3.5 w-3.5" />
                    )}
                  </button>
                ) : (
                  <span className="w-4 shrink-0" />
                )}
                <span
                  className="h-2.5 w-1 rounded-full shrink-0"
                  style={{ backgroundColor: barColor }}
                />
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5 truncate">
                    <span className="text-[12px] font-medium text-[#202124] truncate">{span.name}</span>
                    {isErr && <AlertCircle className="h-3.5 w-3.5 text-[#D93025] shrink-0" />}
                  </div>
                  <div className="text-[11px] font-mono text-[#5F6368] truncate">{span.service_name}</div>
                </div>
              </div>

              <div className="relative px-3 py-2 flex items-center">
                {/* vertical grid ticks */}
                {ticks.slice(1, -1).map((r) => (
                  <div
                    key={r}
                    className="pointer-events-none absolute top-0 bottom-0 border-l border-[#F1F3F4]"
                    style={{ left: `calc(${r * 100}% )` }}
                  />
                ))}
                <div className="relative w-full h-5 flex items-center">
                  <div
                    className="absolute h-3.5 rounded-[3px] transition-all"
                    style={{
                      left: `${leftPct}%`,
                      width: `${widthPct}%`,
                      backgroundColor: barColor,
                      opacity: isSelected ? 1 : 0.88,
                    }}
                  />
                  <span
                    className="absolute text-[11px] font-mono text-[#3C4043] tabular-nums whitespace-nowrap"
                    style={{
                      left: leftPct + widthPct > 78 ? undefined : `calc(${leftPct + widthPct}% + 6px)`,
                      right: leftPct + widthPct > 78 ? `calc(${100 - leftPct}% + 6px)` : undefined,
                    }}
                  >
                    {formatMS(span.duration_ms)}
                  </span>
                </div>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
