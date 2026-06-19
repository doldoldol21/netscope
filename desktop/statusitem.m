#import <Cocoa/Cocoa.h>

// Implemented in Go (//export).
void statusItemClickedGo(void);
void popoverDidHideGo(void);

@interface NSStatusTarget : NSObject
- (void)clicked:(id)sender;
@end
@implementation NSStatusTarget
- (void)clicked:(id)sender { statusItemClickedGo(); }
@end

static NSStatusItem *gItem = nil;
static NSStatusTarget *gTarget = nil;

// installStatusItem creates the menu-bar status item with a template image.
void installStatusItem(const void *png, int len) {
  dispatch_async(dispatch_get_main_queue(), ^{
    [NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
    gItem = [[[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength] retain];
    NSData *d = [NSData dataWithBytes:png length:len];
    NSImage *img = [[NSImage alloc] initWithData:d];
    [img setTemplate:YES];
    [img setSize:NSMakeSize(18, 18)];
    gItem.button.image = img;
    gTarget = [[NSStatusTarget alloc] init];
    gItem.button.target = gTarget;
    gItem.button.action = @selector(clicked:);
  });
}

// statusItemAnchor returns, in top-left screen coordinates (what Wails uses), a
// top-left position for a popover of width winWidth below the status item.
void statusItemAnchor(int winWidth, int *outX, int *outY) {
  NSRect f = gItem.button.window.frame;                 // status item, bottom-left origin
  CGFloat screenH = [[NSScreen mainScreen] frame].size.height;
  int cx = (int)(f.origin.x + f.size.width / 2.0);
  *outX = cx - winWidth / 2;
  *outY = (int)(screenH - f.origin.y);                  // flush under the menu bar
}

// enablePopoverDismiss hides the popover window when it loses key focus (the
// user clicks elsewhere) — the native menu-bar popover behaviour.
static id gResignObs = nil;
void enablePopoverDismiss(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    if (gResignObs) return;
    gResignObs = [[NSNotificationCenter defaultCenter]
      addObserverForName:NSWindowDidResignKeyNotification
      object:nil
      queue:[NSOperationQueue mainQueue]
      usingBlock:^(NSNotification *n) {
        NSWindow *w = (NSWindow *)n.object;
        if (w && w.isVisible) { [w orderOut:nil]; popoverDidHideGo(); }
      }];
  });
}

// focusPopover makes the popover window key so it can later resign (dismiss).
void focusPopover(void) {
  dispatch_async(dispatch_get_main_queue(), ^{
    NSWindow *w = [NSApp keyWindow];
    if (!w) { for (NSWindow *win in [NSApp windows]) { if (win.isVisible) { w = win; break; } } }
    [w makeKeyAndOrderFront:nil];
    [NSApp activateIgnoringOtherApps:YES];
  });
}
