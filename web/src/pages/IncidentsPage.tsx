import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { ChevronDown, ChevronRight, ExternalLink, Terminal } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import {
  fetchIncidentDetail,
  fetchIncidents,
  type IncidentReport,
  type IncidentStatus,
} from '../api/incidents';
import { Banner, EmptyState, StatusChip } from '../components/common/Primitives';
import { formatDateTime, formatMS } from '../utils/format';

const EVENT_TYPE_LABELS: Record<string, string> = {
  upstream_timeout: '504 上游超时',
  upstream_error: '502 连接故障',
  breaker_open: '熔断器开启',
  upstream_5xx: '5xx 突增',
  concurrency_limit: '429 并发满',
  rate_limit_abuse: '429 限流异常',
};

function eventTypeLabel(eventType: string): string {
  return EVENT_TYPE_LABELS[eventType] || eventType || 'unknown';
}

function incidentTitle(item: IncidentReport): string {
  if (item.method && item.path) {
    return `${item.method} ${item.path}`;
  }
  return item.summary || item.scope_key || item.route || item.event_type;
}

function statusChipTone(s: IncidentStatus) {
  switch (s) {
    case 'completed':
      return 'ok';
    case 'failed':
      return 'error';
    case 'processing':
      return 'info';
    default:
      return 'muted';
  }
}

export function IncidentsPage() {
  const { refreshIntervalMs } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();
  const selectedId = searchParams.get('event_id') || '';
  const typeFilter = searchParams.get('type') || '';
  const [showExecutions, setShowExecutions] = useState(true);

  const listQ = useQuery({
    queryKey: ['incidents', typeFilter],
    queryFn: () => fetchIncidents(100, typeFilter),
    refetchInterval: refreshIntervalMs,
    retry: false,
  });

  const detailQ = useQuery({
    queryKey: ['incident-detail', selectedId],
    queryFn: () => fetchIncidentDetail(selectedId),
    enabled: Boolean(selectedId),
    retry: false,
  });

  const incidents = listQ.data?.incidents || [];

  useEffect(() => {
    if (!selectedId && incidents.length > 0) {
      const next: Record<string, string> = { event_id: incidents[0].event_id };
      if (typeFilter) next.type = typeFilter;
      setSearchParams(next, { replace: true });
    }
  }, [selectedId, typeFilter, incidents, setSearchParams]);

  const active = detailQ.data?.incident || incidents.find((i) => i.event_id === selectedId);

  const handleTypeChange = (nextType: string) => {
    const next: Record<string, string> = {};
    if (nextType) next.type = nextType;
    setSearchParams(next);
  };

  const selectIncident = (eventId: string) => {
    const next: Record<string, string> = { event_id: eventId };
    if (typeFilter) next.type = typeFilter;
    setSearchParams(next);
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">AI 故障排查报告 (Incidents)</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            网关异常事件归一化上报并触发 Agent 自动取证、工具调用与中文根因分析报告
          </p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-[12px] text-[#5F6368]">事件类型</label>
          <select
            value={typeFilter}
            onChange={(e) => handleTypeChange(e.target.value)}
            className="rounded border border-[#DADCE0] bg-white px-3 py-1.5 text-[12.5px] text-[#202124] focus:border-[#1A73E8] focus:outline-none"
          >
            <option value="">全部类型</option>
            {Object.entries(EVENT_TYPE_LABELS).map(([value, label]) => (
              <option key={value} value={value}>
                {label} ({value})
              </option>
            ))}
          </select>
        </div>
      </div>

      {listQ.isError && (
        <Banner
          tone="warn"
          title="Agent 服务不可达或鉴权失败"
          message={(listQ.error as Error)?.message || '请检查 Agent 服务运行状态与 JWT audience'}
        />
      )}

      {!listQ.isError && incidents.length === 0 && !listQ.isLoading ? (
        <div className="gc-card">
          <EmptyState
            title="暂无故障排查报告"
            description="当网关触发超时、连接失败、熔断开启、5xx/限流/并发异常并达到阈值后，Agent 将在此生成完整排查报告。"
          />
        </div>
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-[380px_1fr] gap-4 items-start">
          {/* Left Incident List */}
          <div className="gc-card divide-y divide-[#F1F3F4] max-h-[760px] overflow-y-auto">
            {incidents.map((item) => {
              const isSelected = item.event_id === selectedId;
              const occurrences = item.occurrences || 1;
              return (
                <button
                  key={item.event_id}
                  type="button"
                  onClick={() => selectIncident(item.event_id)}
                  className={`w-full text-left px-4 py-3 transition-colors cursor-pointer ${
                    isSelected ? 'bg-[#E8F0FE]/70' : 'hover:bg-[#F8F9FA]'
                  }`}
                >
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-1.5 min-w-0">
                      <StatusChip status={statusChipTone(item.status)} label={item.status} />
                      <StatusChip status="muted" label={eventTypeLabel(item.event_type)} />
                      {occurrences > 1 && (
                        <span className="rounded bg-[#E8F0FE] px-1.5 py-0.5 text-[10.5px] font-mono font-medium text-[#1967D2]">
                          ×{occurrences}
                        </span>
                      )}
                    </div>
                    <span className="text-[11px] text-[#80868B] tabular-nums shrink-0">
                      {formatDateTime(item.occurred_at)}
                    </span>
                  </div>
                  <div className="mt-1.5 font-mono text-[12.5px] font-medium text-[#202124] truncate">
                    {incidentTitle(item)}
                  </div>
                  <div className="mt-1 flex items-center justify-between text-[11.5px] text-[#5F6368] font-mono gap-2">
                    <span className="truncate">{item.scope_key || item.route || '-'}</span>
                    <span className="shrink-0">
                      {item.latency_ms ? `${formatMS(item.latency_ms)} · ` : ''}
                      {item.steps} 步
                    </span>
                  </div>
                </button>
              );
            })}
          </div>

          {/* Right Markdown Report + Executions */}
          {active && (
            <div className="gc-card p-6 space-y-5">
              <div className="border-b border-[#DADCE0] pb-4 flex flex-wrap items-start justify-between gap-3">
                <div>
                  <div className="flex flex-wrap items-center gap-2">
                    <StatusChip status={statusChipTone(active.status)} label={active.status} />
                    <StatusChip status="info" label={eventTypeLabel(active.event_type)} />
                    {(active.occurrences || 1) > 1 && (
                      <StatusChip status="warn" label={`归并 ${active.occurrences} 次`} />
                    )}
                    <h2 className="font-mono text-[16px] font-medium text-[#202124]">
                      {incidentTitle(active)}
                    </h2>
                  </div>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px] text-[#5F6368] font-mono">
                    <span>event_id={active.event_id}</span>
                    <span>scope={active.scope_key || active.route || '-'}</span>
                    {active.request_id && <span>request_id={active.request_id}</span>}
                    {active.status_code ? <span>status={active.status_code}</span> : null}
                    {active.latency_ms ? <span>耗时={formatMS(active.latency_ms)}</span> : null}
                    <span>Tokens={active.total_tokens}</span>
                  </div>
                </div>
                {active.request_id && (
                  <div className="flex items-center gap-2">
                    <Link
                      to={`/logs?request_id=${encodeURIComponent(active.request_id)}`}
                      className="inline-flex items-center gap-1 rounded border border-[#DADCE0] px-2.5 py-1 text-[12px] font-medium text-[#1A73E8] hover:bg-[#F8F9FA]"
                    >
                      关联日志 <ExternalLink className="h-3 w-3" />
                    </Link>
                    <Link
                      to={`/traces?trace_id=${encodeURIComponent(active.request_id)}`}
                      className="inline-flex items-center gap-1 rounded border border-[#DADCE0] px-2.5 py-1 text-[12px] font-medium text-[#1A73E8] hover:bg-[#F8F9FA]"
                    >
                      关联 Trace <ExternalLink className="h-3 w-3" />
                    </Link>
                  </div>
                )}
              </div>

              {active.error && (
                <Banner tone="error" title="排查异常" message={active.error} />
              )}

              {active.final_message ? (
                <div className="gc-markdown text-[13.5px] leading-relaxed">
                  <ReactMarkdown remarkPlugins={[remarkGfm]}>{active.final_message}</ReactMarkdown>
                </div>
              ) : (
                <EmptyState
                  title={active.status === 'processing' ? 'Agent 正在排查中...' : '尚未产出报告正文'}
                />
              )}

              {Array.isArray(active.executions) && active.executions.length > 0 && (
                <div className="border-t border-[#DADCE0] pt-4">
                  <button
                    type="button"
                    onClick={() => setShowExecutions((v) => !v)}
                    className="flex items-center gap-1.5 text-[13px] font-medium text-[#202124] cursor-pointer"
                  >
                    {showExecutions ? (
                      <ChevronDown className="h-4 w-4 text-[#5F6368]" />
                    ) : (
                      <ChevronRight className="h-4 w-4 text-[#5F6368]" />
                    )}
                    <Terminal className="h-4 w-4 text-[#1A73E8]" />
                    工具调用轨迹 ({active.executions.length} 次)
                  </button>

                  {showExecutions && (
                    <div className="mt-3 space-y-2.5">
                      {active.executions.map((ex, idx) => (
                        <div key={idx} className="rounded border border-[#E8EAED] bg-[#F8F9FA] p-3 text-[12px]">
                          <div className="flex items-center justify-between gap-2">
                            <div className="font-mono font-medium text-[#1967D2]">
                              #{ex.step ?? idx + 1} {ex.tool || 'tool'}
                            </div>
                            <div className="flex items-center gap-2 text-[11px] text-[#5F6368] font-mono">
                              {ex.output_truncated && <StatusChip status="warn" label="已截断" />}
                              {ex.duration_ms !== undefined && <span>{formatMS(ex.duration_ms)}</span>}
                            </div>
                          </div>
                          {ex.arguments !== undefined && (
                            <pre className="mt-1.5 rounded bg-white border border-[#E8EAED] p-2 text-[11px] overflow-x-auto">
                              {typeof ex.arguments === 'string'
                                ? ex.arguments
                                : JSON.stringify(ex.arguments, null, 2)}
                            </pre>
                          )}
                          {ex.error && (
                            <div className="mt-1.5 text-[11.5px] text-[#D93025] font-mono">{ex.error}</div>
                          )}
                          {ex.output !== undefined && (
                            <pre className="mt-1.5 max-h-48 overflow-auto rounded bg-white border border-[#E8EAED] p-2 text-[11px]">
                              {typeof ex.output === 'string'
                                ? ex.output
                                : JSON.stringify(ex.output, null, 2)}
                            </pre>
                          )}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
