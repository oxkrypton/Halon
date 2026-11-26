import React, { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { GitBranch, X } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { fetchLogDetail, fetchLogs } from '../api/observability';
import { Banner, EmptyState, HttpStatusBadge, StatusChip } from '../components/common/Primitives';
import { formatDateTime, formatMS, prettyBody } from '../utils/format';

export function LogsPage() {
  const { system, timeWindow, refreshIntervalMs } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();

  const [route, setRoute] = useState(searchParams.get('route') || '');
  const [path, setPath] = useState(searchParams.get('path') || '');
  const [requestId, setRequestId] = useState(searchParams.get('request_id') || '');
  const [traceId, setTraceId] = useState(searchParams.get('trace_id') || '');
  const [clientIp, setClientIp] = useState(searchParams.get('client_ip') || '');
  const [statusBucket, setStatusBucket] = useState<'all' | '2xx' | '4xx' | '5xx'>('all');
  const [minLatency, setMinLatency] = useState('');
  const [offset, setOffset] = useState(0);
  const [selectedId, setSelectedId] = useState<number | null>(null);

  useEffect(() => {
    setRoute(searchParams.get('route') || '');
    setPath(searchParams.get('path') || '');
    setRequestId(searchParams.get('request_id') || '');
    setTraceId(searchParams.get('trace_id') || '');
    setClientIp(searchParams.get('client_ip') || '');
    setOffset(0);
  }, [searchParams]);

  const statusRange =
    statusBucket === '2xx'
      ? { status_min: 200, status_max: 299 }
      : statusBucket === '4xx'
        ? { status_min: 400, status_max: 499 }
        : statusBucket === '5xx'
          ? { status_min: 500, status_max: 599 }
          : {};

  const logsQ = useQuery({
    queryKey: ['logs', timeWindow, route, path, requestId, traceId, clientIp, statusBucket, minLatency, offset],
    queryFn: () =>
      fetchLogs({
        window: timeWindow,
        route: route || undefined,
        path: path || undefined,
        request_id: requestId || undefined,
        trace_id: traceId || undefined,
        client_ip: clientIp || undefined,
        min_latency_ms: minLatency ? Number(minLatency) : undefined,
        ...statusRange,
        limit: 50,
        offset,
      }),
    refetchInterval: refreshIntervalMs,
  });

  const detailQ = useQuery({
    queryKey: ['log-detail', selectedId],
    queryFn: () => fetchLogDetail(selectedId!),
    enabled: selectedId !== null,
  });

  const handleReset = (e: React.FormEvent) => {
    e.preventDefault();
    setSearchParams({});
    setStatusBucket('all');
    setMinLatency('');
    setOffset(0);
  };

  const logs = logsQ.data?.logs || [];
  const total = logsQ.data?.total || 0;
  const detail = detailQ.data?.log;

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-[20px] font-medium text-[#202124]">请求与错误日志 (Logs Explorer)</h1>
        <p className="text-[12.5px] text-[#5F6368]">
          查询全量网关请求日志及慢/错请求报文，可一键下钻至 Trace 瀑布流
        </p>
      </div>

      {/* Filters */}
      <form onSubmit={handleReset} className="gc-card p-3.5 grid grid-cols-2 md:grid-cols-4 xl:grid-cols-7 gap-2.5">
        <select
          value={route}
          onChange={(e) => {
            setRoute(e.target.value);
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12px] font-mono"
        >
          <option value="">全部路由</option>
          {(system?.routes || []).map((r) => (
            <option key={r.prefix} value={r.prefix}>
              {r.prefix}
            </option>
          ))}
        </select>

        <input
          type="text"
          placeholder="精确路径 (/api/...)"
          value={path}
          onChange={(e) => {
            setPath(e.target.value);
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] px-2.5 py-1.5 text-[12px] font-mono"
        />

        <input
          type="text"
          placeholder="Request ID"
          value={requestId}
          onChange={(e) => {
            setRequestId(e.target.value);
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] px-2.5 py-1.5 text-[12px] font-mono"
        />

        <input
          type="text"
          placeholder="Client IP"
          value={clientIp}
          onChange={(e) => {
            setClientIp(e.target.value);
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] px-2.5 py-1.5 text-[12px] font-mono"
        />

        <select
          value={statusBucket}
          onChange={(e) => {
            setStatusBucket(e.target.value as 'all' | '2xx' | '4xx' | '5xx');
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12px]"
        >
          <option value="all">全部状态码</option>
          <option value="2xx">2xx 成功</option>
          <option value="4xx">4xx 客户端错误</option>
          <option value="5xx">5xx 服务端错误</option>
        </select>

        <input
          type="number"
          min={0}
          placeholder="最小耗时 ms"
          value={minLatency}
          onChange={(e) => {
            setMinLatency(e.target.value);
            setOffset(0);
          }}
          className="rounded border border-[#DADCE0] px-2.5 py-1.5 text-[12px] font-mono"
        />

        <button
          type="submit"
          className="rounded border border-[#DADCE0] bg-[#F8F9FA] px-3 py-1.5 text-[12px] font-medium text-[#3C4043] hover:bg-[#F1F3F4] cursor-pointer"
        >
          重置筛选
        </button>
      </form>

      {logsQ.isError && (
        <Banner tone="error" title="查询失败" message={(logsQ.error as Error)?.message || '加载失败'} />
      )}

      <div className="grid grid-cols-1 xl:grid-cols-[1fr_420px] gap-4 items-start">
        <div className={`gc-card overflow-x-auto ${selectedId ? '' : 'xl:col-span-2'}`}>
          {logs.length === 0 ? (
            <EmptyState title="没有匹配的请求日志" />
          ) : (
            <table className="w-full text-left text-[12px]">
              <thead className="bg-[#F8F9FA] text-[11px] text-[#5F6368] border-b border-[#DADCE0]">
                <tr>
                  <th className="px-3.5 py-2 font-medium">时间</th>
                  <th className="px-2.5 py-2 font-medium">状态</th>
                  <th className="px-2.5 py-2 font-medium">方法 &amp; 路径</th>
                  <th className="px-2.5 py-2 font-medium">客户端 IP</th>
                  <th className="px-2.5 py-2 font-medium text-right">总耗时</th>
                  <th className="px-2.5 py-2 font-medium text-right">上游耗时</th>
                  <th className="px-3.5 py-2 font-medium text-right">报文 / Trace</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-[#F1F3F4]">
                {logs.map((item) => (
                  <tr
                    key={item.id}
                    onClick={() => setSelectedId(item.id)}
                    className={`cursor-pointer ${
                      selectedId === item.id ? 'bg-[#E8F0FE]/60' : 'hover:bg-[#F8F9FA]'
                    }`}
                  >
                    <td className="px-3.5 py-2 font-mono text-[11.5px] text-[#5F6368] whitespace-nowrap">
                      {formatDateTime(item.created_at)}
                    </td>
                    <td className="px-2.5 py-2">
                      <HttpStatusBadge code={item.status_code} />
                    </td>
                    <td className="px-2.5 py-2 font-mono text-[#202124] max-w-[280px] truncate">
                      <span className="text-[#5F6368] mr-1">{item.method}</span>
                      {item.path}
                    </td>
                    <td className="px-2.5 py-2 font-mono text-[#5F6368]">{item.client_ip}</td>
                    <td className="px-2.5 py-2 text-right font-mono tabular-nums font-medium">
                      {formatMS(item.latency_ms)}
                    </td>
                    <td className="px-2.5 py-2 text-right font-mono tabular-nums text-[#5F6368]">
                      {item.upstream_latency_ms > 0 ? formatMS(item.upstream_latency_ms) : '—'}
                    </td>
                    <td className="px-3.5 py-2 text-right whitespace-nowrap space-x-1.5">
                      {item.has_body && <StatusChip status="warn" label="Body" />}
                      <Link
                        to={`/traces?trace_id=${encodeURIComponent(item.trace_id || item.request_id)}`}
                        onClick={(e) => e.stopPropagation()}
                        className="inline-flex items-center gap-0.5 text-[#1A73E8] hover:underline font-medium"
                      >
                        <GitBranch className="h-3 w-3" />
                        Trace
                      </Link>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          {/* Pagination */}
          {total > 0 && (
            <div className="flex items-center justify-between px-4 py-2.5 border-t border-[#E8EAED] bg-[#F8F9FA] text-[12px] text-[#5F6368]">
              <span>
                共 {total} 条 · 当前第 {offset + 1}–{Math.min(offset + logs.length, total)} 条
              </span>
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  disabled={offset === 0}
                  onClick={() => setOffset(Math.max(0, offset - 50))}
                  className="rounded border border-[#DADCE0] bg-white px-2.5 py-1 disabled:opacity-40 cursor-pointer"
                >
                  上一页
                </button>
                <button
                  type="button"
                  disabled={offset + 50 >= total}
                  onClick={() => setOffset(offset + 50)}
                  className="rounded border border-[#DADCE0] bg-white px-2.5 py-1 disabled:opacity-40 cursor-pointer"
                >
                  下一页
                </button>
              </div>
            </div>
          )}
        </div>

        {selectedId !== null && (
          <div className="gc-card p-4 space-y-3">
            <div className="flex items-start justify-between gap-2 border-b border-[#E8EAED] pb-2.5">
              <div>
                <div className="text-[11px] uppercase tracking-wide text-[#5F6368]">请求日志详情</div>
                <div className="font-mono text-[13px] font-medium text-[#202124]">
                  {detail ? `${detail.method} ${detail.path}` : `#${selectedId}`}
                </div>
              </div>
              <button
                type="button"
                onClick={() => setSelectedId(null)}
                className="rounded p-1 text-[#5F6368] hover:bg-[#F1F3F4]"
              >
                <X className="h-4 w-4" />
              </button>
            </div>

            {detail && (
              <div className="space-y-3 text-[12px]">
                <div className="grid grid-cols-2 gap-2 rounded bg-[#F8F9FA] p-3 font-mono text-[11.5px]">
                  <div>
                    <span className="text-[#5F6368]">Request ID:</span>
                    <div className="break-all text-[#202124]">{detail.request_id}</div>
                  </div>
                  <div>
                    <span className="text-[#5F6368]">Trace ID:</span>
                    <div className="break-all text-[#202124]">{detail.trace_id || '未启用 OTel'}</div>
                  </div>
                  <div>
                    <span className="text-[#5F6368]">Route:</span>
                    <div className="text-[#202124]">{detail.route || '—'}</div>
                  </div>
                  <div>
                    <span className="text-[#5F6368]">Upstream:</span>
                    <div className="text-[#202124]">
                      {detail.upstream_result || '—'} · retries={detail.retries}
                    </div>
                  </div>
                </div>

                <Link
                  to={`/traces?trace_id=${encodeURIComponent(detail.trace_id || detail.request_id)}`}
                  className="inline-flex items-center gap-1.5 rounded bg-[#1A73E8] px-3 py-1.5 text-[12px] font-medium text-white hover:bg-[#1765CC]"
                >
                  <GitBranch className="h-3.5 w-3.5" />
                  在 Trace 瀑布流中打开
                </Link>

                <div>
                  <div className="text-[11.5px] font-medium text-[#5F6368] mb-1">Request Body</div>
                  <pre className="max-h-48 overflow-auto rounded border border-[#E8EAED] bg-[#F8F9FA] p-2.5 text-[11.5px]">
                    {detail.request_body ? prettyBody(detail.request_body) : '（未捕获请求体或为空）'}
                  </pre>
                </div>

                <div>
                  <div className="text-[11.5px] font-medium text-[#5F6368] mb-1">Response Body</div>
                  <pre className="max-h-56 overflow-auto rounded border border-[#E8EAED] bg-[#F8F9FA] p-2.5 text-[11.5px]">
                    {detail.response_body ? prettyBody(detail.response_body) : '（未捕获响应体或为空）'}
                  </pre>
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
