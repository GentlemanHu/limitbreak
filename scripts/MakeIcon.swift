import Foundation
import CoreGraphics
import ImageIO
import UniformTypeIdentifiers

let destination = URL(fileURLWithPath: CommandLine.arguments[1])
var family = Data("icns".utf8)
family.append(contentsOf: [0, 0, 0, 0])
for (size, kind) in [(256, "ic08"), (512, "ic09"), (1024, "ic10")] {
    let space = CGColorSpaceCreateDeviceRGB()
    guard let context = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: size * 4, space: space, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { exit(1) }
    context.scaleBy(x: CGFloat(size) / 1024, y: CGFloat(size) / 1024)
    let rect = CGRect(x: 64, y: 64, width: 896, height: 896)
    context.setFillColor(CGColor(red: 0.13, green: 0.28, blue: 0.58, alpha: 1))
    context.addPath(CGPath(roundedRect: rect, cornerWidth: 196, cornerHeight: 196, transform: nil))
    context.fillPath()
    context.setStrokeColor(CGColor(red: 0.86, green: 0.94, blue: 1, alpha: 1))
    context.setLineWidth(33)
    context.setLineCap(.round)
    for (y, x) in [(CGFloat(310), CGFloat(350)), (CGFloat(512), CGFloat(675)), (CGFloat(714), CGFloat(475))] {
        context.move(to: CGPoint(x: 230, y: y)); context.addLine(to: CGPoint(x: 790, y: y)); context.strokePath()
        context.setFillColor(CGColor(red: 0.13, green: 0.28, blue: 0.58, alpha: 1))
        context.fillEllipse(in: CGRect(x: x - 66, y: y - 66, width: 132, height: 132))
        context.setFillColor(CGColor(red: 0.91, green: 0.96, blue: 1, alpha: 1))
        context.fillEllipse(in: CGRect(x: x - 42, y: y - 42, width: 84, height: 84))
    }
    guard let image = context.makeImage() else { exit(1) }
    let encoded = NSMutableData()
    guard let writer = CGImageDestinationCreateWithData(encoded, UTType.png.identifier as CFString, 1, nil) else { exit(1) }
    CGImageDestinationAddImage(writer, image, nil)
    guard CGImageDestinationFinalize(writer) else { exit(1) }
    family.append(Data(kind.utf8))
    var length = UInt32(encoded.length + 8).bigEndian
    family.append(Data(bytes: &length, count: 4))
    family.append(encoded as Data)
}
var length = UInt32(family.count).bigEndian
family.replaceSubrange(4..<8, with: Data(bytes: &length, count: 4))
try family.write(to: destination)
