import { ProfileDropdown } from '@/components/profile-dropdown'
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem } from '@/components/ui/sidebar'

// Sidebar footer: the operator menu (identity + sign-out).
export function NavUser() {
  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <SidebarMenuButton size='lg' asChild>
          <div className='flex items-center justify-between'>
            <div className='flex flex-col'>
              <span className='truncate text-sm font-medium'>operator</span>
              <span className='text-muted-foreground truncate text-xs'>admin token</span>
            </div>
            <ProfileDropdown />
          </div>
        </SidebarMenuButton>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
