import { useMemo } from 'react';
import type * as echarts from 'echarts';
import type { TraceSummary } from '../../api/traces';
import { formatMS } from '../../utils/format';
import { EChartBox } from './EChartBox';

export function TraceScatter({
  traces,
  onSelectTrace,
  height = 240,
}: {
  traces: TraceSummary[];
  onSelectTrace: (traceId: string) => void;
  height?: number;
}) {
  const option = useMemo<echarts.EChartsOption>(() => {
    const okData = traces
      .filter((t) => !t.has_error)
      .map((t) => ({
        value: [new Date(t.start_time).getTime(), Number(t.duration_ms.toFixed(2))],
        traceId: t.trace_id,
        operation: t.root_operation,
        service: t.root_service,
        spans: t.span_count,
      }));
    const errData = traces
      .filter((t) => t.has_error)
      .map((t) => ({
        value: [new Date(t.start_time).getTime(), Number(t.duration_ms.toFixed(2))],
        traceId: t.trace_id,
        operation: t.root_operation,
        service: t.root_service,
        spans: t.span_count,
      }));

    return {
      tooltip: {
        trigger: 'item',
        backgroundColor: '#ffffff',
        borderColor: '#dadce0',
        borderWidth: 1,
        textStyle: { color: '#202124', fontSize: 12 },
        formatter: (params: unknown) => {
          const item = params as {
            data?: {
              value: [number, number];
              traceId: string;
              operation: string;
              service: string;
              spans: number;
            };
          };
          const d = item.data;
          if (!d) return '';
          const ts = new Date(d.value[0]).toLocaleTimeString('zh-CN', { hour12: false });
          return `<div style="font-family:var(--gc-font-sans)">
            <div style="font-weight:600;margin-bottom:2px">${d.operation}</div>
            <div style="color:#5f6368;font-size:11px">${d.service} · ${d.spans} spans · ${ts}</div>
            <div style="margin-top:4px;font-family:var(--gc-font-mono)">耗时: <b>${formatMS(d.value[1])}</b></div>
            <div style="color:#1a73e8;font-size:11px;margin-top:2px">点击查看 Span 瀑布流</div>
          </div>`;
        },
      },
      legend: {
        top: 0,
        right: 8,
        textStyle: { color: '#5f6368', fontSize: 11.5 },
        data: ['正常', '错误 / 超时'],
      },
      grid: { left: 56, right: 20, top: 32, bottom: 26 },
      xAxis: {
        type: 'time',
        axisLine: { lineStyle: { color: '#dadce0' } },
        axisLabel: { color: '#5f6368', fontSize: 11 },
      },
      yAxis: {
        type: 'value',
        name: 'ms',
        nameTextStyle: { color: '#80868b', fontSize: 11 },
        axisLabel: { color: '#5f6368', fontSize: 11 },
        splitLine: { lineStyle: { color: '#f1f3f4', type: 'dashed' } },
      },
      series: [
        {
          name: '正常',
          type: 'scatter',
          symbolSize: 8,
          itemStyle: { color: 'rgba(26, 115, 232, 0.78)' },
          data: okData,
        },
        {
          name: '错误 / 超时',
          type: 'scatter',
          symbolSize: 10,
          itemStyle: { color: '#D93025' },
          data: errData,
        },
      ],
    };
  }, [traces]);

  return (
    <EChartBox
      option={option}
      height={height}
      onPointClick={(params) => {
        const p = params as { data?: { traceId?: string } };
        if (p?.data?.traceId) {
          onSelectTrace(p.data.traceId);
        }
      }}
    />
  );
}
