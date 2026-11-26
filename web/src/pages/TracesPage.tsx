import React, { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useSearchParams } from 'react-router-dom';
import { ArrowLeft, Search } from 'lucide-react';
import { useAuth } from '../auth/AuthContext';
import { fetchTraceDetail, fetchTraces, type TraceSpan } from '../api/traces';
import { TraceScatter } from '../components/Charts/TraceScatter';
import { WaterfallView } from '../components/TraceWaterfall/WaterfallView';
import { SpanInspector } from '../components/TraceWaterfall/SpanInspector';
import { Banner, EmptyState, HttpStatusBadge, StatusChip } from '../components/common/Primitives';
import { formatDateTime, formatMS } from '../utils/format';

export function TracesPage() {
  const { system, timeWindow, refreshIntervalMs } = useAuth();
  const [searchParams, setSearchParams] = useSearchParams();

  const activeTraceId = searchParams.get('trace_id') || '';
  const [route, setRoute] = useState('');
  const [minDuration, setMinDuration] = useState('');
  const [onlyErrors, setOnlyErrors] = useState(false);
  const [sourcePref, setSourcePref] = useState<'auto' | 'tempo' | 'postgres'>('auto');
  const [traceLookup, setTraceLookup] = useState(activeTraceId);
  const [selectedSpan, setSelectedSpan] = useState<TraceSpan | null>(null);

  useEffect(() => {
    setTraceLookup(activeTraceId);
    setSelectedSpan(null);
  }, [activeTraceId]);

  const tracesQ = useQuery({
    queryKey: ['traces', timeWindow, route, minDuration, onlyErrors, sourcePref],
    queryFn: () =>
      fetchTraces({
        window: timeWindow,
        route: route || undefined,
        min_duration_ms: minDuration ? Number(minDuration) : undefined,
        only_errors: onlyErrors,
        source: sourcePref,
        limit: 80,
      }),
    refetchInterval: activeTraceId ? false : refreshIntervalMs,
  });

  const detailQ = useQuery({
    queryKey: ['trace-detail', activeTraceId],
    queryFn: () => fetchTraceDetail(activeTraceId),
    enabled: Boolean(activeTraceId),
  });

  useEffect(() => {
    if (detailQ.data?.trace?.spans?.length && !selectedSpan) {
      setSelectedSpan(detailQ.data.trace.spans[0]);
    }
  }, [detailQ.data, selectedSpan]);

  const openTrace = (id: string) => {
    setSearchParams({ trace_id: id });
  };

  const handleLookupSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = traceLookup.trim();
    if (trimmed) {
      openTrace(trimmed);
    } else {
      setSearchParams({});
    }
  };

  if (activeTraceId) {
    const trace = detailQ.data?.trace;
    return (
      <div className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex items-center gap-3">
            <button
              type="button"
              onClick={() => setSearchParams({})}
              className="inline-flex items-center gap-1.5 rounded border border-[#DADCE0] bg-white px-3 py-1.5 text-[12.5px] font-medium text-[#3C4043] hover:bg-[#F1F3F4] cursor-pointer"
            >
              <ArrowLeft className="h-3.5 w-3.5" />
              返回 Trace 列表
            </button>
            <div>
              <div className="flex items-center gap-2">
                <h1 className="text-[17px] font-medium text-[#202124] font-mono">{activeTraceId}</h1>
                {detailQ.data?.source && (
                  <StatusChip
                    status={detailQ.data.source === 'tempo' ? 'info' : 'muted'}
                    label={detailQ.data.source === 'tempo' ? 'Tempo 分布式链路' : 'PG 网关瀑布流'}
                  />
                )}
              </div>
              {trace && (
                <p className="text-[12px] text-[#5F6368]">
                  {trace.root_operation} · {formatDateTime(trace.start_time)} · 总耗时{' '}
                  <span className="font-mono font-medium text-[#202124]">{formatMS(trace.duration_ms)}</span>
                </p>
              )}
            </div>
          </div>
        </div>

        {detailQ.isLoading && (
          <div className="gc-card p-8 text-center text-[#5F6368]">正在加载 Span 瀑布流...</div>
        )}

        {detailQ.isError && (
          <Banner
            tone="error"
            title="加载 Trace 失败"
            message={(detailQ.error as Error)?.message || '未找到对应链路记录'}
          />
        )}

        {trace && (
          <div className="grid grid-cols-1 xl:grid-cols-[1fr_400px] gap-4 items-start">
            <WaterfallView
              trace={trace}
              selectedSpanId={selectedSpan?.span_id || null}
              onSelectSpan={(sp) => setSelectedSpan(sp)}
            />
            {selectedSpan && (
              <SpanInspector
                trace={trace}
                span={selectedSpan}
                onClose={() => setSelectedSpan(null)}
              />
            )}
          </div>
        )}
      </div>
    );
  }

  const traces = tracesQ.data?.traces || [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-[20px] font-medium text-[#202124]">链路追踪 (Traces)</h1>
          <p className="text-[12.5px] text-[#5F6368]">
            类 Jaeger 耗时散点分布与 Span 瀑布流分析 — 支持 Tempo 分布式追踪及 PostgreSQL 自动降级
          </p>
        </div>
        {tracesQ.data?.source && (
          <StatusChip
            status={tracesQ.data.source === 'tempo' ? 'info' : 'muted'}
            label={tracesQ.data.source === 'tempo' ? '数据源: Tempo' : '数据源: PostgreSQL (网关+上游)'}
          />
        )}
      </div>

      {/* Filter Bar */}
      <div className="gc-card p-3.5 flex flex-wrap items-center gap-3">
        <form onSubmit={handleLookupSubmit} className="relative flex-1 min-w-[220px]">
          <Search className="absolute left-3 top-1/2 -translate-y-1/2 h-3.5 w-3.5 text-[#5F6368]" />
          <input
            type="text"
            value={traceLookup}
            onChange={(e) => setTraceLookup(e.target.value)}
            placeholder="输入 Trace ID 或 Request ID 直接下钻"
            className="w-full rounded border border-[#DADCE0] pl-8 pr-3 py-1.5 text-[12.5px] font-mono focus:border-[#1A73E8] focus:outline-none"
          />
        </form>

        <select
          value={route}
          onChange={(e) => setRoute(e.target.value)}
          className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12.5px] font-mono"
        >
          <option value="">全部路由</option>
          {(system?.routes || []).map((r) => (
            <option key={r.prefix} value={r.prefix}>
              {r.prefix}
            </option>
          ))}
        </select>

        <input
          type="number"
          min={0}
          step={10}
          placeholder="最小耗时 ms"
          value={minDuration}
          onChange={(e) => setMinDuration(e.target.value)}
          className="w-32 rounded border border-[#DADCE0] px-2.5 py-1.5 text-[12.5px] font-mono focus:border-[#1A73E8] focus:outline-none"
        />

        <label className="inline-flex items-center gap-1.5 text-[12.5px] text-[#3C4043] cursor-pointer select-none">
          <input
            type="checkbox"
            checked={onlyErrors}
            onChange={(e) => setOnlyErrors(e.target.checked)}
            className="rounded border-[#DADCE0] text-[#1A73E8]"
          />
          仅看错误 / 异常
        </label>

        <select
          value={sourcePref}
          onChange={(e) => setSourcePref(e.target.value as 'auto' | 'tempo' | 'postgres')}
          className="rounded border border-[#DADCE0] bg-white px-2.5 py-1.5 text-[12.5px]"
        >
          <option value="auto">自动选择数据源</option>
          <option value="postgres">PostgreSQL 日志</option>
          {system?.capabilities?.tempo_enabled && <option value="tempo">Tempo 分布式</option>}
        </select>
      </div>

      {tracesQ.isError && (
        <Banner tone="error" title="查询失败" message={(tracesQ.error as Error)?.message || '无法加载 Trace'} />
      )}

      {/* Scatter plot */}
      <div className="gc-card p-4">
        <div className="flex items-center justify-between mb-1">
          <h2 className="text-[13.5px] font-medium text-[#202124]">耗时散点分布 (点击散点打开瀑布流)</h2>
          <span className="text-[11.5px] text-[#5F6368] tabular-nums">展示最近 {traces.length} 条</span>
        </div>
        {traces.length === 0 ? (
          <EmptyState title="暂无符合条件的链路" description="尝试放宽最小耗时或切换时间范围" />
        ) : (
          <TraceScatter traces={traces} onSelectTrace={openTrace} />
        )}
      </div>

      {/* Trace list table */}
      <div className="gc-card overflow-hidden">
        <table className="w-full text-left text-[12.5px]">
          <thead className="bg-[#F8F9FA] text-[11.5px] text-[#5F6368] border-b border-[#DADCE0]">
            <tr>
              <th className="px-4 py-2.5 font-medium">操作 / 路径</th>
              <th className="px-3 py-2.5 font-medium">服务 / 路由</th>
              <th className="px-3 py-2.5 font-medium">Trace / Request ID</th>
              <th className="px-3 py-2.5 font-medium text-right">Spans</th>
              <th className="px-3 py-2.5 font-medium text-center">状态</th>
              <th className="px-3 py-2.5 font-medium text-right">总耗时</th>
              <th className="px-4 py-2.5 font-medium text-right">时间</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-[#F1F3F4]">
            {traces.map((t) => (
              <tr
                key={`${t.trace_id}-${t.start_time}`}
                onClick={() => openTrace(t.trace_id)}
                className="hover:bg-[#E8F0FE]/40 cursor-pointer"
              >
                <td className="px-4 py-2.5 font-mono text-[#1A73E8] font-medium truncate max-w-[280px]">
                  {t.root_operation}
                </td>
                <td className="px-3 py-2.5 font-mono text-[#5F6368]">{t.root_service}</td>
                <td className="px-3 py-2.5 font-mono text-[11.5px] text-[#5F6368]">
                  {t.trace_id.slice(0, 16)}…
                </td>
                <td className="px-3 py-2.5 text-right font-mono tabular-nums">{t.span_count}</td>
                <td className="px-3 py-2.5 text-center">
                  {t.status_code ? (
                    <HttpStatusBadge code={t.status_code} />
                  ) : (
                    <StatusChip status={t.has_error ? 'error' : 'ok'} label={t.has_error ? 'error' : 'ok'} />
                  )}
                </td>
                <td
                  className={`px-3 py-2.5 text-right font-mono tabular-nums font-medium ${
                    t.has_error ? 'text-[#D93025]' : 'text-[#202124]'
                  }`}
                >
                  {formatMS(t.duration_ms)}
                </td>
                <td className="px-4 py-2.5 text-right text-[11.5px] text-[#5F6368] tabular-nums">
                  {formatDateTime(t.start_time)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
