import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { ArrowRight } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { fetchOverview, fetchRouteStats, fetchTimeSeries } from '../api/observability';
import { fetchBreakers } from '../api/gateway';
import { fetchIncidents } from '../api/incidents';
import { LatencyChart } from '../components/Charts/LatencyChart';
import { TrafficChart } from '../components/Charts/TrafficChart';
import { Banner, EmptyState, Scorecard, StatusChip } from '../components/common/Primitives';
import { formatCount, formatDateTime, formatMS, formatPercent } from '../utils/format';

export function OverviewPage() {
  const { system, timeWindow, refreshIntervalMs } = useAuth();
  const [routeFilter, setRouteFilter] = useState('');

  const overviewQ = useQuery({
    queryKey: ['overview', timeWindow, routeFilter],
    queryFn: () => fetchOverview(timeWindow, routeFilter),
    refetchInterval: refreshIntervalMs,
  });

  const seriesQ = useQuery({
    queryKey: ['timeseries', timeWindow, routeFilter],
    queryFn: () => fetchTimeSeries(timeWindow, routeFilter),
    refetchInterval: refreshIntervalMs,
  });

  const topRoutesQ = useQuery({
    queryKey: ['routes-top', timeWindow],
    queryFn: () => fetchRouteStats(timeWindow, 'route', 'p99_ms', 5),
    refetchInterval: refreshIntervalMs,
  });

  const breakersQ = useQuery({
    queryKey: ['breakers'],
    queryFn: fetchBreakers,
    refetchInterval: refreshIntervalMs,
  });

  const incidentsQ = useQuery({
    queryKey: ['incidents-recent'],
    queryFn: () => fetchIncidents(5),
    refetchInterval: refreshIntervalMs,
    retry: false,
  });

  const ov = overviewQ.data?.overview;
  const rt = overviewQ.data?.runtime;
  const queueDropped = (rt?.trafficlog_dropped ?? 0) + (rt?.incident_events_dropped ?? 0);
  const webhookFailures = rt?.webhook_notify_failures ?? 0;
  const openBreakers = (breakersQ.data?.breakers || []).filter((b) => b.state === 'open').length;

  return (
    <div className="space-y-5">
      {/* Page header */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">观测概览</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            网关全局吞吐、P50 / P95 / P99 分位数延迟与故障排查态势
          </p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-[12px] text-[#5F6368]">路由筛选</label>
          <select
            value={routeFilter}
            onChange={(e) => setRouteFilter(e.target.value)}
            className="rounded border border-[#DADCE0] bg-white px-3 py-1.5 text-[12.5px] font-mono text-[#202124] focus:border-[#1A73E8] focus:outline-none"
          >
            <option value="">全部路由</option>
            {(system?.routes || []).map((r) => (
              <option key={r.prefix} value={r.prefix}>
                {r.prefix}
              </option>
            ))}
          </select>
        </div>
      </div>

      {!system?.health?.redis_up && (
        <Banner
          tone="warn"
          title="Redis 连接降级"
          message="当前副本报告共享状态 Redis 不可达，黑名单写操作受限、限流处于 fail-open 保护状态。"
        />
      )}

      {overviewQ.isError && (
        <Banner
          tone="error"
          title="查询失败"
          message={(overviewQ.error as Error)?.message || '无法加载概览指标'}
        />
      )}

      {/* 6 Google Scorecards */}
      <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-6 gap-3.5">
        <Scorecard
          label="总请求量"
          value={formatCount(ov?.total_requests)}
          sub={`QPS ${(ov?.qps ?? 0).toFixed(2)} · ${ov?.unique_clients ?? 0} IP`}
          tone="blue"
        />
        <Scorecard
          label="P99 延迟"
          value={formatMS(ov?.p99_ms)}
          sub={`Max ${formatMS(ov?.max_ms)}`}
          tone={(ov?.p99_ms ?? 0) >= 1000 ? 'red' : 'default'}
        />
        <Scorecard
          label="P95 延迟"
          value={formatMS(ov?.p95_ms)}
          sub={`Avg ${formatMS(ov?.avg_ms)}`}
        />
        <Scorecard label="P50 延迟" value={formatMS(ov?.p50_ms)} sub="中位响应时间" />
        <Scorecard
          label="5xx 错误率"
          value={formatPercent(ov?.error_rate)}
          sub={`5xx: ${formatCount(ov?.status_5xx)} · 4xx: ${formatCount(ov?.status_4xx)}`}
          tone={(ov?.error_rate ?? 0) >= 0.05 ? 'red' : (ov?.status_5xx ?? 0) > 0 ? 'yellow' : 'green'}
        />
        <Scorecard
          label="熔断开启"
          value={String(openBreakers)}
          sub={`总计 ${(breakersQ.data?.breakers || []).length} 条路由 (当前副本)`}
          tone={openBreakers > 0 ? 'red' : 'green'}
        />
      </div>

      {/* Runtime Protection & Queue Health (current replica) */}
      <div>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-[12.5px] font-medium text-[#5F6368]">
            运行态保护与队列健康（当前副本）
          </h2>
        </div>
        <div className="grid grid-cols-2 md:grid-cols-3 xl:grid-cols-5 gap-3.5">
          <Scorecard
            label="进行中请求"
            value={formatCount(rt?.in_flight_requests)}
            sub={`上游在途 ${formatCount(rt?.upstream_in_flight)}`}
            tone="blue"
          />
          <Scorecard
            label="网关前置拒绝"
            value={formatCount(rt?.rejected?.total)}
            sub={`黑名单 ${formatCount(rt?.rejected?.blacklist)} · 限流 ${formatCount(rt?.rejected?.rate_limit)} · 并发 ${formatCount(rt?.rejected?.concurrency_limit)} · 体大 ${formatCount(rt?.rejected?.body_too_large)}`}
            tone={(rt?.rejected?.total ?? 0) > 0 ? 'yellow' : 'default'}
          />
          <Scorecard
            label="上游重试"
            value={formatCount(rt?.retries_total)}
            sub={`预算耗尽 ${formatCount(rt?.retry_exhausted_total)} · 熔断触发 ${formatCount(rt?.breaker_trips_total)}`}
            tone={(rt?.retry_exhausted_total ?? 0) > 0 ? 'yellow' : 'default'}
          />
          <Scorecard
            label="队列丢弃"
            value={formatCount(queueDropped)}
            sub={`日志 ${formatCount(rt?.trafficlog_dropped)} · 故障事件 ${formatCount(rt?.incident_events_dropped)}`}
            tone={queueDropped > 0 ? 'red' : 'green'}
          />
          <Scorecard
            label="Webhook 推送失败"
            value={formatCount(webhookFailures)}
            sub="故障事件投递 Agent"
            tone={webhookFailures > 0 ? 'red' : 'green'}
          />
        </div>
      </div>

      {/* Main Charts */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <div className="gc-card p-4">
          <div className="flex items-center justify-between mb-2">
            <div>
              <h2 className="text-[14px] font-medium text-[#202124]">延迟分位数趋势 (P50 / P95 / P99)</h2>
              <p className="text-[11.5px] text-[#5F6368]">PostgreSQL 精确分位数按时间桶聚合</p>
            </div>
            <Link to="/routes" className="text-[12px] text-[#1A73E8] hover:underline flex items-center gap-1">
              路由详情 <ArrowRight className="h-3.5 w-3.5" />
            </Link>
          </div>
          {(seriesQ.data?.points?.length || 0) === 0 ? (
            <EmptyState title="当前窗口暂无请求数据" description="尝试切换更大时间范围或发送测试流量" />
          ) : (
            <LatencyChart points={seriesQ.data?.points || []} />
          )}
        </div>

        <div className="gc-card p-4">
          <div className="flex items-center justify-between mb-2">
            <div>
              <h2 className="text-[14px] font-medium text-[#202124]">请求速率与状态码分布</h2>
              <p className="text-[11.5px] text-[#5F6368]">2xx / 4xx / 5xx 分桶统计</p>
            </div>
            <Link to="/logs" className="text-[12px] text-[#1A73E8] hover:underline flex items-center gap-1">
              日志探索 <ArrowRight className="h-3.5 w-3.5" />
            </Link>
          </div>
          {(seriesQ.data?.points?.length || 0) === 0 ? (
            <EmptyState title="当前窗口暂无请求数据" />
          ) : (
            <TrafficChart points={seriesQ.data?.points || []} />
          )}
        </div>
      </div>

      {/* Bottom Tables: Top slow routes + Recent AI Incidents */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        <div className="gc-card overflow-hidden">
          <div className="flex items-center justify-between px-4 py-3 border-b border-[#DADCE0]">
            <h2 className="text-[14px] font-medium text-[#202124]">Top 慢路由 (按 P99)</h2>
            <Link to="/routes" className="text-[12px] text-[#1A73E8] hover:underline">
              查看全部
            </Link>
          </div>
          {(topRoutesQ.data?.routes?.length || 0) === 0 ? (
            <EmptyState title="暂无路由统计" />
          ) : (
            <table className="w-full text-left text-[12.5px]">
              <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#E8EAED]">
                <tr>
                  <th className="px-4 py-2 font-medium">路由</th>
                  <th className="px-3 py-2 font-medium text-right">请求数</th>
                  <th className="px-3 py-2 font-medium text-right">P95</th>
                  <th className="px-3 py-2 font-medium text-right">P99</th>
                  <th className="px-4 py-2 font-medium text-right">5xx 率</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-[#F1F3F4]">
                {(topRoutesQ.data?.routes || []).map((r) => (
                  <tr key={r.target} className="hover:bg-[#F8F9FA]">
                    <td className="px-4 py-2.5 font-mono text-[#202124]">{r.target}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatCount(r.requests)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums">{formatMS(r.p95_ms)}</td>
                    <td className="px-3 py-2.5 text-right font-mono tabular-nums font-medium text-[#D93025]">
                      {formatMS(r.p99_ms)}
                    </td>
                    <td className="px-4 py-2.5 text-right font-mono tabular-nums">{formatPercent(r.error_rate)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        <div className="gc-card overflow-hidden">
          <div className="flex items-center justify-between px-4 py-3 border-b border-[#DADCE0]">
            <h2 className="text-[14px] font-medium text-[#202124]">最近 AI 故障报告</h2>
            <Link to="/incidents" className="text-[12px] text-[#1A73E8] hover:underline">
              查看全部
            </Link>
          </div>
          {incidentsQ.isError ? (
            <EmptyState
              title="Agent 接口未连接或未授权"
              description="若已部署 Agent 服务且处于 jwt 模式，请使用包含 halon-agent audience 的令牌。"
            />
          ) : (incidentsQ.data?.incidents?.length || 0) === 0 ? (
            <EmptyState title="暂无故障排查事件" description="网关捕捉到异常并达标上报后，Agent 将自动生成排查报告。" />
          ) : (
            <div className="divide-y divide-[#F1F3F4]">
              {(incidentsQ.data?.incidents || []).map((inc) => (
                <Link
                  key={inc.event_id}
                  to={`/incidents?event_id=${encodeURIComponent(inc.event_id)}`}
                  className="flex items-center justify-between px-4 py-2.5 hover:bg-[#F8F9FA]"
                >
                  <div className="min-w-0 pr-3">
                    <div className="flex items-center gap-2">
                      <StatusChip
                        status={
                          inc.status === 'completed'
                            ? 'ok'
                            : inc.status === 'failed'
                              ? 'error'
                              : inc.status === 'processing'
                                ? 'info'
                                : 'muted'
                        }
                        label={inc.status}
                      />
                      <StatusChip status="muted" label={inc.event_type || 'incident'} />
                      <span className="font-mono text-[12.5px] text-[#202124] truncate">
                        {inc.method && inc.path
                          ? `${inc.method} ${inc.path}`
                          : inc.summary || inc.scope_key || inc.route || inc.event_type}
                      </span>
                    </div>
                    <div className="mt-0.5 text-[11px] text-[#5F6368] font-mono">
                      {inc.scope_key || inc.route || '-'}
                      {(inc.occurrences || 1) > 1 ? ` · ×${inc.occurrences}` : ''}
                      {inc.latency_ms ? ` · ${formatMS(inc.latency_ms)}` : ''} · {inc.steps} 步
                    </div>
                  </div>
                  <span className="text-[11.5px] text-[#80868B] tabular-nums shrink-0">
                    {formatDateTime(inc.occurred_at)}
                  </span>
                </Link>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
