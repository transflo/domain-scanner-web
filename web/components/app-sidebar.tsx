"use client"

import Link from "next/link"
import { usePathname, useRouter } from "next/navigation"
import {
  FileTextIcon,
  LayoutDashboardIcon,
  ListChecksIcon,
  LogOutIcon,
  RadarIcon,
  SettingsIcon,
  TableIcon,
} from "lucide-react"

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar"
import { Badge } from "@/components/ui/badge"
import { useHealth } from "@/hooks/use-health"
import { api } from "@/lib/api"

const items = [
  { href: "/", label: "仪表盘", icon: LayoutDashboardIcon },
  { href: "/jobs", label: "扫描任务", icon: ListChecksIcon },
  { href: "/results", label: "扫描结果", icon: TableIcon },
  { href: "/logs", label: "运行日志", icon: FileTextIcon },
  { href: "/settings", label: "设置", icon: SettingsIcon },
]

export function AppSidebar() {
  const pathname = usePathname()
  const router = useRouter()
  const { connected } = useHealth()

  async function logout() {
    try {
      await api.logout()
    } finally {
      router.replace("/login")
      router.refresh()
    }
  }

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" render={<Link href="/" />}>
              <div className="flex size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
                <RadarIcon className="size-4" />
              </div>
              <div className="grid flex-1 text-left text-sm leading-tight">
                <span className="truncate font-medium">Domain Scanner</span>
                <span className="truncate text-xs text-muted-foreground">未注册域名扫描</span>
              </div>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupContent>
            <SidebarMenu>
              {items.map((item) => {
                const active = item.href === "/" ? pathname === "/" : pathname.startsWith(item.href)
                return (
                  <SidebarMenuItem key={item.href}>
                    <SidebarMenuButton
                      isActive={active}
                      tooltip={item.label}
                      render={<Link href={item.href} />}
                    >
                      <item.icon />
                      <span>{item.label}</span>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                )
              })}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <div
              className="flex items-center justify-between px-2 py-1 group-data-[collapsible=icon]:hidden"
              data-testid="connection-status"
            >
              <span className="text-xs text-muted-foreground">后端连接</span>
              <Badge variant={connected === false ? "destructive" : "secondary"}>
                {connected === undefined ? "检测中" : connected ? "正常" : "已断开"}
              </Badge>
            </div>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton tooltip="退出登录" onClick={logout}>
              <LogOutIcon />
              <span>退出登录</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  )
}
