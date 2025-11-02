import type {
  SystemStatus,
  AccountInfo,
  Position,
  DecisionRecord,
  Statistics,
  TraderInfo,
  CompetitionData,
} from '../types';

const API_BASE = '/api';

export const api = {
  // Competition related endpoints
  async getCompetition(): Promise<CompetitionData> {
    const res = await fetch(`${API_BASE}/competition`);
    if (!res.ok) throw new Error('Failed to fetch competition data');
    return res.json();
  },

  async getTraders(): Promise<TraderInfo[]> {
    const res = await fetch(`${API_BASE}/traders`);
    if (!res.ok) throw new Error('Failed to fetch trader list');
    return res.json();
  },

  // Get system status (supports trader_id)
  async getStatus(traderId?: string): Promise<SystemStatus> {
    const url = traderId
      ? `${API_BASE}/status?trader_id=${traderId}`
      : `${API_BASE}/status`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch system status');
    return res.json();
  },

  // Get account information (supports trader_id)
  async getAccount(traderId?: string): Promise<AccountInfo> {
    const url = traderId
      ? `${API_BASE}/account?trader_id=${traderId}`
      : `${API_BASE}/account`;
    const res = await fetch(url, {
      cache: 'no-store',
      headers: {
        'Cache-Control': 'no-cache',
      },
    });
    if (!res.ok) throw new Error('Failed to fetch account information');
    const data = await res.json();
    console.log('Account data fetched:', data);
    return data;
  },

  // Get position list (supports trader_id)
  async getPositions(traderId?: string): Promise<Position[]> {
    const url = traderId
      ? `${API_BASE}/positions?trader_id=${traderId}`
      : `${API_BASE}/positions`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch position list');
    return res.json();
  },

  // Get decision logs (supports trader_id)
  async getDecisions(traderId?: string): Promise<DecisionRecord[]> {
    const url = traderId
      ? `${API_BASE}/decisions?trader_id=${traderId}`
      : `${API_BASE}/decisions`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch decision logs');
    return res.json();
  },

  // Get latest decisions (supports trader_id)
  async getLatestDecisions(traderId?: string): Promise<DecisionRecord[]> {
    const url = traderId
      ? `${API_BASE}/decisions/latest?trader_id=${traderId}`
      : `${API_BASE}/decisions/latest`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch latest decisions');
    return res.json();
  },

  // Get statistics (supports trader_id)
  async getStatistics(traderId?: string): Promise<Statistics> {
    const url = traderId
      ? `${API_BASE}/statistics?trader_id=${traderId}`
      : `${API_BASE}/statistics`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch statistics');
    return res.json();
  },

  // Get equity history data (supports trader_id)
  async getEquityHistory(traderId?: string): Promise<any[]> {
    const url = traderId
      ? `${API_BASE}/equity-history?trader_id=${traderId}`
      : `${API_BASE}/equity-history`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch historical data');
    return res.json();
  },

  // Get AI learning performance analysis (supports trader_id)
  async getPerformance(traderId?: string): Promise<any> {
    const url = traderId
      ? `${API_BASE}/performance?trader_id=${traderId}`
      : `${API_BASE}/performance`;
    const res = await fetch(url);
    if (!res.ok) throw new Error('Failed to fetch AI learning data');
    return res.json();
  },
};
