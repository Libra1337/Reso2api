import { HashRouter, Navigate, Route, Routes } from "react-router-dom";
import AppLayout from "@/components/layout/AppLayout";
import LoginPage from "@/pages/LoginPage";
import CatsPage from "@/pages/CatsPage";
import DashboardPage from "@/pages/DashboardPage";
import AccountsPage from "@/pages/AccountsPage";
import ApiPage from "@/pages/ApiPage";
import RequestLogsPage from "@/pages/RequestLogsPage";
import LimitsPage from "@/pages/LimitsPage";
import TasksPage from "@/pages/TasksPage";
import FirewallPage from "@/pages/FirewallPage";
import UsagePage from "@/pages/UsagePage";
import LogsPage from "@/pages/LogsPage";

function NotFoundPage() {
  return (
    <div className="flex h-screen flex-col items-center justify-center gap-6">
      <div className="text-7xl font-bold text-muted-foreground/30">404</div>
      <p className="text-lg text-muted-foreground">页面不存在</p>
    </div>
  );
}

export default function App() {
  return (
    <HashRouter>
      <Routes>
        <Route path="/" element={<Navigate to="/admin/dashboard" replace />} />
        <Route path="/admin/login" element={<LoginPage />} />
        <Route path="/admin" element={<AppLayout />}>
          <Route index element={<Navigate to="/admin/dashboard" replace />} />
          <Route path="dashboard" element={<DashboardPage />} />
          <Route path="cats" element={<CatsPage />} />
          <Route path="token" element={<AccountsPage />} />
          <Route path="keys" element={<ApiPage />} />
          <Route path="reqlog" element={<RequestLogsPage />} />
          <Route path="limits" element={<LimitsPage />} />
          <Route path="tasks" element={<TasksPage />} />
          <Route path="usage" element={<UsagePage />} />
          <Route path="firewall" element={<FirewallPage />} />
          <Route path="logs" element={<LogsPage />} />
        </Route>
        <Route path="*" element={<NotFoundPage />} />
      </Routes>
    </HashRouter>
  );
}
