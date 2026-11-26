import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { X } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { fetchLogs, fetchRouteStats, fetchTimeSeries } from '../api/observability';
import { LatencyChart } from '../components/Charts/LatencyChart';
import { Banner, EmptyState, HttpStatusBadge } from '../components/common/Primitives';
import { formatCount, formatDateTime, formatMS, formatPercent } from '../utils/format';

export function RoutesPage() {
  const { timeWindow, refreshIntervalMs } = useAuth();
  const [groupBy, setGroupBy] = useState<'route' | 'path'>('route');
  const [sortBy, setSortBy] = useState('p99_ms');
  const [selectedTarget, setSelectedTarget] = useState<string | null>(null);

  const routesQ = useQuery({
    queryKey: ['route-stats', timeWindow, groupBy, sortBy],
    queryFn: () => fetchRouteStats(timeWindow, groupBy, sortBy, 50),
    refetchInterval: refreshIntervalMs,
  });

  const detailSeriesQ = useQuery({
    queryKey: ['route-detail-series', timeWindow, selectedTarget],
    queryFn: () => fetchTimeSeries(timeWindow, groupBy === 'route' ? selectedTarget || '' : ''),
    enabled: Boolean(selectedTarget && groupBy === 'route'),
  });

  const detailLogsQ = useQuery({
    queryKey: ['route-detail-logs', timeWindow, groupBy, selectedTarget],
    queryFn: () =>
      fetchLogs({
        window: timeWindow,
        route: groupBy === 'route' ? selectedTarget || undefined : undefined,
        path: groupBy === 'path' ? selectedTarget || undefined : undefined,
        limit: 15,
      }),
    enabled: Boolean(selectedTarget),
  });

  const rows = routesQ.data?.routes || [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">路由与分位数延迟分析</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            按网关路由前缀或精确请求路径统计 Avg / P50 / P95 / P99 / Max 与 5xx 错误率
          </p>
        </div>

        <div className="flex items-center gap-2">
          <div className="inline-flex rounded border border-[#DADCE0] bg-white p-0.5 text-[12px]">
            <button
              type="button"
              onClick={() => {
                setGroupBy('route');
                setSelectedTarget(null);
              }}
              className={`rounded px-3 py-1 font-medium cursor-pointer ${
                groupBy === 'route' ? 'bg-[#E8F0FE] text-[#1967D2]' : 'text-[#5F6368]'
              }`}
            >
              按路由前缀
            </button>
            <button
              type="button"
              onClick={() => {
                setGroupBy('path');
                setSelectedTarget(null);
              }}
              className={`rounded px-3 py-1 font-medium cursor-pointer ${
                groupBy === 'path' ? 'bg-[#E8F0FE] text-[#1967D2]' : 'text-[#5F6368]'
              }`}
            >
              按精确路径
            </button>
          </div>

          <select
            value={sortBy}
            onChange={(e) => setSortBy(e.target.value)}
            className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12.5px]"
          >
            <option value="p99_ms">按 P99 降序</option>
            <option value="p95_ms">按 P95 降序</option>
            <option value="requests">按请求量降序</option>
            <option value="error_rate">按 5xx 错误率降序</option>
            <option value="avg_ms">按平均耗时降序</option>
          </select>
        </div>
      </div>

      {routesQ.isError && (
        <Banner tone="error" title="查询失败" message={(routesQ.error as Error)?.message || '加载失败'} />
      )}

      <div className="grid grid-cols-1 xl:grid-cols-[1fr_420px] gap-4 items-start">
        <div className={`gc-card overflow-x-auto ${selectedTarget ? '' : 'xl:col-span-2'}`}>
          {rows.length === 0 ? (
            <EmptyState title="暂无路由统计数据" />
          ) : (
            <table className="w-full text-left text-[12.5px]">
              <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#DADCE0]">
                <tr>
                  <th className="px-4 py-2.5 font-medium">{groupBy === 'route' ? '路由前缀' : '请求路径'}</th>
                  <th className="px-3 py-2.5 font-medium text-right">请求数</th>
                  <th className="px-3 py-2.5 font-medium text-right">Avg</th>
                  <th className="px-3 py-2.5 font-medium text-right">P50</th>
                  <th className="px-3 py-2.5 font-medium text-right">P95</th>
                  <th className="px-3 py-2.5 font-medium text-right">P99</th>
                  <th className="px-3 py-2.5 font-medium text-right">Max</th>
                  <th className="px-3 py-2.5 font-medium text-right">4xx</th>
                  <th className="px-4 py-2.5 font-medium text-right">5xx 率</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-[#F1F3F4]">
                {rows.map((r) => (
                  <tr
                    key={r.target}
                    onClick={() => setSelectedTarget(r.target)}
                    className={`cursor-pointer ${
                      selectedTarget === r.target ? 'bg-[#E8F0FE]/60' : 'hover:bg-[#F8F9FA]'
                    }`}
                  >
                    <td className="px-4 py-2.5 font-mono text-[#1A73E8] font-medium">{r.target}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatCount(r.requests)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatMS(r.avg_ms)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatMS(r.p50_ms)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatMS(r.p95_ms)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums font-medium text-[#D93025]">
                      {formatMS(r.p99_ms)}
                    </td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums text-[#5F6368]">
                      {formatMS(r.max_ms)}
                    </td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{r.status_4xx}</td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{formatPercent(r.error_rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        {selectedTarget && (
          <div className="gc-card p-4 space-y-4">
            <div className="flex items-start justify-between gap-2 border-b border-[#E8EAED] pb-3">
              <div className="min-w-0">
                <div className="text-[11px] uppercase tracking-wide text-[#5F6368]">下钻详情</div>
                <div className="font-mono text-[14px] font-medium text-[#202124] break-all">
                  {selectedTarget}
                </div>
              </div>
              <button
                type="button"
                onClick={() => setSelectedTarget(null)}
                className="rounded p-1 text-[#5F6368] hover:bg-[#F1F3F4]"
              >
                <X className="h-4 w-4" />
              </button>
            </div>

            {groupBy === 'route' && (detailSeriesQ.data?.points?.length || 0) > 0 && (
              <div>
                <div className="text-[12px] font-medium text-[#5F6368] mb-1">分位数延迟曲线</div>
                <LatencyChart points={detailSeriesQ.data?.points || []} height={200} />
              </div>
            )}

            <div>
              <div className="flex items-center justify-between mb-2">
                <span className="text-[12px] font-medium text-[#5F6368]">最近请求样本</span>
                <Link
                  to={`/logs?${groupBy}=${encodeURIComponent(selectedTarget)}`}
                  className="text-[12px] text-[#1A73E8] hover:underline"
                >
                  去日志页筛选
                </Link>
              </div>
              <div className="divide-y divide-[#F1F3F4] border border-[#E8EAED] rounded">
                {(detailLogsQ.data?.logs || []).map((l) => (
                  <Link
                    key={l.id}
                    to={`/traces?trace_id=${encodeURIComponent(l.trace_id || l.request_id)}`}
                    className="flex items-center justify-between px-3 py-2 text-[12px] hover:bg-[#F8F9FA]"
                  >
                    <div className="flex items-center gap-2 min-w-0">
                      <HttpStatusBadge code={l.status_code} />
                      <span className="font-mono truncate">{l.method} {l.path}</span>
                    </div>
                    <div className="text-right font-mono tabular-nums shrink-0 ml-2">
                      <div>{formatMS(l.latency_ms)}</div>
                      <div className="text-[10.5px] text-[#80868B]">{formatDateTime(l.created_at)}</div>
                    </div>
                  </Link>
                ))}
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
