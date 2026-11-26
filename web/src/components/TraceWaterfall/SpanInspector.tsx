import { Link } from 'react-router-dom';
import { ExternalLink, X } from 'lucide-react';
import type { TraceDetail, TraceSpan } from '../../api/traces';
import { formatDateTime, formatMS, prettyBody } from '../../utils/format';
import { HttpStatusBadge, StatusChip } from '../common/Primitives';

export function SpanInspector({
  trace,
  span,
  onClose,
}: {
  trace: TraceDetail;
  span: TraceSpan;
  onClose: () => void;
}) {
  const attrEntries = Object.entries(span.attributes || {});
  const logs = trace.logs || [];

  return (
    <div className="gc-card flex flex-col overflow-hidden">
      <div className="flex items-start justify-between gap-2 border-b border-[#DADCE0] bg-[#F8F9FA] px-4 py-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="text-[13.5px] font-medium text-[#202124] truncate">{span.name}</span>
            <StatusChip
              status={span.status === 'error' ? 'error' : span.status === 'ok' ? 'ok' : 'muted'}
              label={span.status}
            />
          </div>
          <div className="mt-0.5 text-[11.5px] font-mono text-[#5F6368]">
            {span.service_name} · span_id={span.span_id}
          </div>
        </div>
        <button
          type="button"
          onClick={onClose}
          className="rounded p-1 text-[#5F6368] hover:bg-[#E8EAED]"
          aria-label="关闭检查器"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      <div className="p-4 space-y-4 max-h-[680px] overflow-y-auto">
        <div className="grid grid-cols-3 gap-2 text-[12px]">
          <div className="rounded bg-[#F8F9FA] px-3 py-2">
            <div className="text-[11px] text-[#5F6368]">开始偏移</div>
            <div className="font-mono font-medium tabular-nums">{formatMS(span.offset_ms)}</div>
          </div>
          <div className="rounded bg-[#F8F9FA] px-3 py-2">
            <div className="text-[11px] text-[#5F6368]">耗时</div>
            <div className="font-mono font-medium tabular-nums">{formatMS(span.duration_ms)}</div>
          </div>
          <div className="rounded bg-[#F8F9FA] px-3 py-2">
            <div className="text-[11px] text-[#5F6368]">Kind</div>
            <div className="font-mono font-medium">{span.kind || 'INTERNAL'}</div>
          </div>
        </div>

        {span.status_message && (
          <div className="rounded-md bg-[#FCE8E6] px-3 py-2 text-[12px] text-[#C5221F] font-mono">
            {span.status_message}
          </div>
        )}

        <div>
          <h4 className="text-[12px] font-medium text-[#5F6368] mb-2">Attributes / Tags</h4>
          {attrEntries.length === 0 ? (
            <div className="text-[12px] text-[#80868B]">无附加属性</div>
          ) : (
            <div className="rounded border border-[#E8EAED] divide-y divide-[#F1F3F4]">
              {attrEntries.map(([k, v]) => (
                <div key={k} className="grid grid-cols-[140px_1fr] gap-2 px-3 py-1.5 text-[12px]">
                  <span className="font-mono text-[#5F6368] truncate">{k}</span>
                  <span className="font-mono text-[#202124] break-all">{String(v)}</span>
                </div>
              ))}
            </div>
          )}
        </div>

        {span.events && span.events.length > 0 && (
          <div>
            <h4 className="text-[12px] font-medium text-[#5F6368] mb-2">Span Events</h4>
            <div className="space-y-2">
              {span.events.map((ev, idx) => (
                <div key={idx} className="rounded border border-[#E8EAED] p-2.5 text-[12px]">
                  <div className="font-medium text-[#202124]">{ev.name}</div>
                  {ev.attributes && (
                    <pre className="mt-1 text-[11px] text-[#5F6368] whitespace-pre-wrap">
                      {JSON.stringify(ev.attributes, null, 2)}
                    </pre>
                  )}
                </div>
              ))}
            </div>
          </div>
        )}

        {logs.length > 0 && (
          <div>
            <h4 className="text-[12px] font-medium text-[#5F6368] mb-2">关联网关日志 &amp; 报文</h4>
            <div className="space-y-3">
              {logs.map((l) => (
                <div key={l.id} className="rounded border border-[#DADCE0] p-3 space-y-2">
                  <div className="flex flex-wrap items-center justify-between gap-2 text-[12px]">
                    <div className="flex items-center gap-2">
                      <HttpStatusBadge code={l.status_code} />
                      <span className="font-mono font-medium">
                        {l.method} {l.path}
                      </span>
                    </div>
                    <span className="text-[11px] text-[#5F6368] tabular-nums">
                      {formatDateTime(l.created_at)}
                    </span>
                  </div>
                  <div className="flex flex-wrap items-center gap-3 text-[11.5px] text-[#5F6368] font-mono">
                    <span>req_id={l.request_id}</span>
                    <span>总耗时={formatMS(l.latency_ms)}</span>
                    {l.upstream_latency_ms > 0 && <span>上游={formatMS(l.upstream_latency_ms)}</span>}
                    {l.retries > 0 && <span>重试={l.retries}</span>}
                    <Link
                      to={`/logs?request_id=${encodeURIComponent(l.request_id)}`}
                      className="inline-flex items-center gap-1 text-[#1A73E8] hover:underline"
                    >
                      在日志页打开 <ExternalLink className="h-3 w-3" />
                    </Link>
                  </div>
                  {l.request_body && (
                    <div>
                      <div className="text-[11px] font-medium text-[#5F6368] mb-1">Request Body</div>
                      <pre className="max-h-36 overflow-auto rounded bg-[#F8F9FA] p-2 text-[11px] border border-[#E8EAED]">
                        {prettyBody(l.request_body)}
                      </pre>
                    </div>
                  )}
                  {l.response_body && (
                    <div>
                      <div className="text-[11px] font-medium text-[#5F6368] mb-1">Response Body</div>
                      <pre className="max-h-36 overflow-auto rounded bg-[#F8F9FA] p-2 text-[11px] border border-[#E8EAED]">
                        {prettyBody(l.response_body)}
                      </pre>
                    </div>
                  )}
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
