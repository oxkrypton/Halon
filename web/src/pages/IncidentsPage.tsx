import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { ChevronDown, ChevronRight, ExternalLink, Terminal } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { fetchIncidentDetail, fetchIncidents, type IncidentStatus } from '../api/incidents';
import { Banner, EmptyState, StatusChip } from '../components/common/Primitives';
import { formatDateTime, formatMS } from '../utils/format';

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
  const [showExecutions, setShowExecutions] = useState(true);

  const listQ = useQuery({
    queryKey: ['incidents'],
    queryFn: () => fetchIncidents(100),
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
      setSearchParams({ event_id: incidents[0].event_id }, { replace: true });
    }
  }, [selectedId, incidents, setSearchParams]);

  const active = detailQ.data?.incident || incidents.find((i) => i.event_id === selectedId);

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-[20px] font-medium text-[#202124]">AI 故障排查报告 (Incidents)</h1>
        <p className="text-[12.5px] text-[#5F6368]">
          网关 504 超时触发 Agent 自动取证、工具调用与中文根因分析报告
        </p>
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
            title="暂无超时故障排查报告"
            description="当上游响应超时触发 504 时，网关会通过 Webhook 上报事件，Agent 将在此生成完整排查报告。"
          />
        </div>
      ) : (
        <div className="grid grid-cols-1 lg:grid-cols-[360px_1fr] gap-4 items-start">
          {/* Left Incident List */}
          <div className="gc-card divide-y divide-[#F1F3F4] max-h-[760px] overflow-y-auto">
            {incidents.map((item) => {
              const isSelected = item.event_id === selectedId;
              return (
                <button
                  key={item.event_id}
                  type="button"
                  onClick={() => setSearchParams({ event_id: item.event_id })}
                  className={`w-full text-left px-4 py-3 transition-colors cursor-pointer ${
                    isSelected ? 'bg-[#E8F0FE]/70' : 'hover:bg-[#F8F9FA]'
                  }`}
                >
                  <div className="flex items-center justify-between gap-2">
                    <StatusChip status={statusChipTone(item.status)} label={item.status} />
                    <span className="text-[11px] text-[#80868B] tabular-nums">
                      {formatDateTime(item.occurred_at)}
                    </span>
                  </div>
                  <div className="mt-1.5 font-mono text-[12.5px] font-medium text-[#202124] truncate">
                    {item.method} {item.path}
                  </div>
                  <div className="mt-1 flex items-center justify-between text-[11.5px] text-[#5F6368] font-mono">
                    <span>{item.route}</span>
                    <span>
                      {formatMS(item.latency_ms)} · {item.steps} 步
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
                  <div className="flex items-center gap-2">
                    <StatusChip status={statusChipTone(active.status)} label={active.status} />
                    <h2 className="font-mono text-[16px] font-medium text-[#202124]">
                      {active.method} {active.path}
                    </h2>
                  </div>
                  <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-[12px] text-[#5F6368] font-mono">
                    <span>event_id={active.event_id}</span>
                    <span>request_id={active.request_id}</span>
                    <span>耗时={formatMS(active.latency_ms)}</span>
                    <span>Tokens={active.total_tokens}</span>
                  </div>
                </div>
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
