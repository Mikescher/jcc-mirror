/** The number formatting the whole dashboard shares. It is deliberately the same
 *  set of answers the Go `format` package gives, so a summary line written by the
 *  daemon and a column rendered here do not disagree about what 1.5 GiB is. */

const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

export function bytes(n: number | undefined | null): string {
  if (!n) return '0 B';
  let value = Math.abs(n);
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  const sign = n < 0 ? '-' : '';
  return unit === 0 ? `${sign}${value} B` : `${sign}${value.toFixed(2)} ${units[unit]}`;
}

/** rate is in bits per second, like the Go side: link speeds are quoted in bits
 *  and file sizes in bytes, and mixing them is how a cap gets set eight times
 *  too high. */
export function rate(bytesMoved: number, seconds: number): string {
  if (!seconds || seconds <= 0) return '-';
  const bps = (bytesMoved * 8) / seconds;
  if (bps >= 1e9) return `${(bps / 1e9).toFixed(2)} Gbit/s`;
  if (bps >= 1e6) return `${(bps / 1e6).toFixed(2)} Mbit/s`;
  if (bps >= 1e3) return `${(bps / 1e3).toFixed(2)} kbit/s`;
  return `${bps.toFixed(0)} bit/s`;
}

export function comma(n: number | undefined | null): string {
  return (n ?? 0).toLocaleString('en-US');
}

/** count is a number with its noun, pluralised. "1 files" reads as a bug in a
 *  view whose whole job is to be believed. */
export function count(n: number, noun: string): string {
  return `${comma(n)} ${noun}${n === 1 ? '' : 's'}`;
}

export function duration(seconds: number): string {
  if (!isFinite(seconds) || seconds < 0) return '-';
  if (seconds >= 86400) {
    const days = Math.floor(seconds / 86400);
    const hours = Math.floor((seconds % 86400) / 3600);
    return hours ? `${days}d${hours}h` : `${days}d`;
  }
  if (seconds >= 3600) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    return `${hours}h${String(minutes).padStart(2, '0')}m`;
  }
  if (seconds >= 60) {
    const minutes = Math.floor(seconds / 60);
    return `${minutes}m${String(Math.floor(seconds % 60)).padStart(2, '0')}s`;
  }
  return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`;
}

/** since is how long ago a timestamp was, for the "5m ago" columns. */
export function since(ts: string | undefined): string {
  if (!ts) return '-';
  return duration((Date.now() - Date.parse(ts)) / 1000);
}

export function clock(ts: string | undefined): string {
  if (!ts) return '-';
  const d = new Date(ts);
  return d.toLocaleString(undefined, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function timeOnly(ts: string | undefined): string {
  if (!ts) return '-';
  return new Date(ts).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function percent(done: number, total: number): number {
  if (!total || total <= 0 || done <= 0) return 0;
  return Math.min(100, Math.round((done / total) * 100));
}

/** cap renders a schedule cell's limit, where 0 means no cap rather than none. */
export function cap(limit: number): string {
  return limit === 0 ? 'no limit' : rate(limit, 1);
}
