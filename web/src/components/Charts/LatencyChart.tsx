import { useMemo } from 'react';
import type * as echarts from 'echarts';
import type { TimeSeriesPoint } from '../../api/observability';
import { EChartBox } from './EChartBox';

export function LatencyChart({ points, height = 260 }: { points: TimeSeriesPoint[]; height?: number }) {
  const option = useMemo<echarts.EChartsOption>(() => {
    const times = points.map((p) => {
      const d = new Date(p.ts);
      return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    });
    return {
      color: ['#1A73E8', '#F9AB00', '#D93025'],
      tooltip: {
        trigger: 'axis',
        backgroundColor: '#ffffff',
        borderColor: '#dadce0',
        borderWidth: 1,
        textStyle: { color: '#202124', fontSize: 12 },
        axisPointer: { type: 'cross', lineStyle: { color: '#bdc1c6' } },
      },
      legend: {
        top: 0,
        right: 8,
        itemWidth: 12,
        itemHeight: 3,
        textStyle: { color: '#5f6368', fontSize: 11.5 },
        data: ['P50', 'P95', 'P99'],
      },
      grid: { left: 48, right: 16, top: 32, bottom: 26 },
      xAxis: {
        type: 'category',
        boundaryGap: false,
        data: times,
        axisLine: { lineStyle: { color: '#dadce0' } },
        axisLabel: { color: '#5f6368', fontSize: 11 },
      },
      yAxis: {
        type: 'value',
        name: 'ms',
        nameTextStyle: { color: '#80868b', fontSize: 11, padding: [0, 24, 0, 0] },
        axisLabel: { color: '#5f6368', fontSize: 11 },
        splitLine: { lineStyle: { color: '#f1f3f4', type: 'dashed' } },
      },
      series: [
        {
          name: 'P50',
          type: 'line',
          smooth: true,
          showSymbol: false,
          lineStyle: { width: 2 },
          data: points.map((p) => Number(p.p50_ms.toFixed(2))),
        },
        {
          name: 'P95',
          type: 'line',
          smooth: true,
          showSymbol: false,
          lineStyle: { width: 2 },
          data: points.map((p) => Number(p.p95_ms.toFixed(2))),
        },
        {
          name: 'P99',
          type: 'line',
          smooth: true,
          showSymbol: false,
          lineStyle: { width: 2.2 },
          areaStyle: { color: 'rgba(217, 48, 37, 0.06)' },
          data: points.map((p) => Number(p.p99_ms.toFixed(2))),
        },
      ],
    };
  }, [points]);

  return <EChartBox option={option} height={height} />;
}
