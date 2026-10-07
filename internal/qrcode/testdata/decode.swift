import Foundation
import CoreImage

let context = CIContext(options: [.useSoftwareRenderer: true])
let detector = CIDetector(ofType: CIDetectorTypeQRCode, context: context, options: [CIDetectorAccuracy: CIDetectorAccuracyHigh])!
// macOS sandboxes may block the graphics facilities required by this reader.
// Establish that the reader works by decoding an image generated independently
// by Apple's own encoder before attempting the application-generated image.
let referenceText = "https://example.com/qr-reader-check"
let generator = CIFilter(name: "CIQRCodeGenerator", parameters: ["inputMessage": Data(referenceText.utf8), "inputCorrectionLevel": "M"])!
let reference = generator.outputImage!.transformed(by: CGAffineTransform(scaleX: 10, y: 10))
let white = CIImage(color: CIColor.white).cropped(to: reference.extent.insetBy(dx: -40, dy: -40))
let available = detector.features(in: reference.composited(over: white)).contains {
    ($0 as? CIQRCodeFeature)?.messageString == referenceText
}
let image = CIImage(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))!
let payloads = available ? detector.features(in: image).compactMap { ($0 as? CIQRCodeFeature)?.messageString } : []
struct Result: Encodable {
    let available: Bool
    let payloads: [String]
}
let encoded = try JSONEncoder().encode(Result(available: available, payloads: payloads))
FileHandle.standardOutput.write(encoded)
