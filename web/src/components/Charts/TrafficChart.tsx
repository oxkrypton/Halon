import { useMemo } from 'react';
import type * as echarts from 'echarts';
import type { TimeSeriesPoint } from '../../api/observability';
import { EChartBox } from './EChartBox';

export function TrafficChart({ points, height = 260 }: { points: TimeSeriesPoint[]; height?: number }) {
  const option = useMemo<echarts.EChartsOption>(() => {
    const times = points.map((p) => {
      const d = new Date(p.ts);
      return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
    });
    return {
      color: ['#1E8E3E', '#F9AB00', '#D93025'],
      tooltip: {
        trigger: 'axis',
        backgroundColor: '#ffffff',
        borderColor: '#dadce0',
        borderWidth: 1,
        textStyle: { color: '#202124', fontSize: 12 },
      },
      legend: {
        top: 0,
        right: 8,
        itemWidth: 10,
        itemHeight: 10,
        textStyle: { color: '#5f6368', fontSize: 11.5 },
        data: ['2xx', '4xx', '5xx'],
      },
      grid: { left: 48, right: 16, top: 32, bottom: 26 },
      xAxis: {
        type: 'category',
        data: times,
        axisLine: { lineStyle: { color: '#dadce0' } },
        axisLabel: { color: '#5f6368', fontSize: 11 },
      },
      yAxis: {
        type: 'value',
        axisLabel: { color: '#5f6368', fontSize: 11 },
        splitLine: { lineStyle: { color: '#f1f3f4', type: 'dashed' } },
      },
      series: [
        {
          name: '2xx',
          type: 'bar',
          stack: 'status',
          barMaxWidth: 16,
          data: points.map((p) => p.status_2xx),
        },
        {
          name: '4xx',
          type: 'bar',
          stack: 'status',
          barMaxWidth: 16,
          data: points.map((p) => p.status_4xx),
        },
        {
          name: '5xx',
          type: 'bar',
          stack: 'status',
          barMaxWidth: 16,
          data: points.map((p) => p.status_5xx),
        },
      ],
    };
  }, [points]);

  return <EChartBox option={option} height={height} />;
}
