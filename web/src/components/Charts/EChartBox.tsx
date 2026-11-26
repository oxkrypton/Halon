import { useEffect, useRef } from 'react';
import * as echarts from 'echarts';

export function EChartBox({
  option,
  height = 260,
  onPointClick,
}: {
  option: echarts.EChartsOption;
  height?: number;
  onPointClick?: (params: unknown) => void;
}) {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<echarts.ECharts | null>(null);
  const clickHandlerRef = useRef(onPointClick);
  clickHandlerRef.current = onPointClick;

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const chart = echarts.init(el);
    chartRef.current = chart;

    const handleClick = (params: unknown) => {
      clickHandlerRef.current?.(params);
    };
    chart.on('click', handleClick);

    const observer = new ResizeObserver(() => {
      chart.resize();
    });
    observer.observe(el);

    return () => {
      observer.disconnect();
      chart.off('click', handleClick);
      chart.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    chartRef.current?.setOption(option, { notMerge: true });
  }, [option]);

  return <div ref={containerRef} style={{ width: '100%', height }} />;
}
