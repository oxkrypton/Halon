import React from 'react';
import { AlertTriangle, Inbox } from 'lucide-react';

export function Scorecard({
  label,
  value,
  sub,
  tone = 'default',
}: {
  label: string;
  value: string;
  sub?: string;
  tone?: 'default' | 'blue' | 'green' | 'yellow' | 'red';
}) {
  const accent = {
    default: 'text-[#202124]',
    blue: 'text-[#1A73E8]',
    green: 'text-[#1E8E3E]',
    yellow: 'text-[#B06000]',
    red: 'text-[#D93025]',
  }[tone];

  return (
    <div className="gc-card px-4 py-3.5">
      <div className="text-[11.5px] font-medium text-[#5F6368] tracking-wide">{label}</div>
      <div className={`mt-1 text-[22px] font-medium tabular-nums leading-tight ${accent}`}>{value}</div>
      {sub && <div className="mt-1 text-[11.5px] text-[#80868B] tabular-nums">{sub}</div>}
    </div>
  );
}

export function StatusChip({
  status,
  label,
}: {
  status: 'ok' | 'warn' | 'error' | 'info' | 'muted';
  label: string;
}) {
  const cls = {
    ok: 'bg-[#E6F4EA] text-[#137333]',
    warn: 'bg-[#FEF7E0] text-[#B06000]',
    error: 'bg-[#FCE8E6] text-[#C5221F]',
    info: 'bg-[#E8F0FE] text-[#1967D2]',
    muted: 'bg-[#F1F3F4] text-[#5F6368]',
  }[status];

  return (
    <span className={`inline-flex items-center rounded-full px-2 py-0.5 text-[11px] font-medium ${cls}`}>
      {label}
    </span>
  );
}

export function HttpStatusBadge({ code }: { code: number }) {
  const status = code >= 500 ? 'error' : code >= 400 ? 'warn' : code >= 200 ? 'ok' : 'muted';
  return <StatusChip status={status} label={String(code)} />;
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="flex flex-col items-center justify-center py-12 px-4 text-center">
      <div className="h-10 w-10 rounded-full bg-[#F1F3F4] flex items-center justify-center text-[#5F6368] mb-3">
        <Inbox className="h-5 w-5" />
      </div>
      <div className="text-[14px] font-medium text-[#202124]">{title}</div>
      {description && <div className="mt-1 max-w-md text-[12.5px] text-[#5F6368]">{description}</div>}
      {action && <div className="mt-4">{action}</div>}
    </div>
  );
}

export function Banner({
  tone = 'warn',
  title,
  message,
}: {
  tone?: 'warn' | 'error' | 'info';
  title?: string;
  message: string;
}) {
  const styles = {
    warn: 'bg-[#FEF7E0] border-[#F9AB00]/40 text-[#8D5000]',
    error: 'bg-[#FCE8E6] border-[#D93025]/30 text-[#A50E0E]',
    info: 'bg-[#E8F0FE] border-[#1A73E8]/30 text-[#1967D2]',
  }[tone];

  return (
    <div className={`flex items-start gap-2.5 rounded-lg border px-4 py-2.5 text-[12.5px] ${styles}`}>
      <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
      <div>
        {title && <span className="font-medium mr-1.5">{title}:</span>}
        <span>{message}</span>
      </div>
    </div>
  );
}
